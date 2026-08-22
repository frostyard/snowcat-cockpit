package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/profile"
)

const (
	StatusAllocating = "allocating"
	StatusRunning    = "running"
	StatusExited     = "exited"
	StatusFailed     = "failed"
	StatusStopped    = "stopped"
	StatusCleaned    = "cleaned"

	recordVersion = 1
	maxOutput     = 64 * 1024
)

var (
	ErrInvalid   = errors.New("invalid managed-worker request")
	ErrNotFound  = errors.New("managed worker not found")
	ErrConflict  = errors.New("managed-worker lifecycle conflict")
	ErrNotReady  = errors.New("provider profile is not ready")
	workerIDRE   = regexp.MustCompile(`^worker-[0-9a-f]{16}$`)
	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	commitRE     = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	interfaceRE  = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
)

type LaunchRequest struct {
	Provider   string `json:"provider"`
	Role       string `json:"role"`
	Repository string `json:"repository"`
	Source     string `json:"source"`
	BaseRef    string `json:"baseRef,omitempty"`
}

type Record struct {
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	NodeID     string     `json:"nodeId"`
	Provider   string     `json:"provider"`
	Role       string     `json:"role"`
	Repository string     `json:"repository"`
	Source     string     `json:"source"`
	Workspace  string     `json:"workspace"`
	BaseRef    string     `json:"baseRef"`
	BaseCommit string     `json:"baseCommit"`
	Branch     string     `json:"branch"`
	Status     string     `json:"status"`
	Detail     string     `json:"detail"`
	CreatedAt  time.Time  `json:"createdAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	StoppedAt  *time.Time `json:"stoppedAt,omitempty"`
	CleanedAt  *time.Time `json:"cleanedAt,omitempty"`
}

type Console struct {
	URL string `json:"url"`
}

type consoleProcess struct {
	command *exec.Cmd
	url     string
}

type Command struct {
	Name      string
	Arguments []string
	Directory string
	Env       []string
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	process := exec.CommandContext(ctx, command.Name, command.Arguments...)
	process.Dir = command.Directory
	process.Env = command.Env
	var output limitedBuffer
	process.Stdout = &output
	process.Stderr = &output
	err := process.Run()
	if output.overflow {
		return nil, errors.New("command output exceeded managed-worker limit")
	}
	return output.Bytes(), err
}

type Config struct {
	StateDirectory string
	NodeID         string
	Runner         Runner
	Ready          func(string) error
	LookPath       func(string) (string, error)
	Now            func() time.Time
	Random         io.Reader
	Environment    func() []string
}

type Manager struct {
	stateDirectory string
	nodeID         string
	runner         Runner
	ready          func(string) error
	lookPath       func(string) (string, error)
	now            func() time.Time
	random         io.Reader
	environment    func() []string
	consoles       map[string]*consoleProcess
	mutex          sync.Mutex
}

func New(config Config) (*Manager, error) {
	if config.StateDirectory == "" || config.NodeID == "" {
		return nil, fmt.Errorf("%w: state directory and node ID are required", ErrInvalid)
	}
	if config.Runner == nil {
		config.Runner = OSRunner{}
	}
	if config.Ready == nil {
		config.Ready = func(string) error { return nil }
	}
	if config.LookPath == nil {
		config.LookPath = exec.LookPath
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Environment == nil {
		config.Environment = os.Environ
	}
	parentEnvironment := config.Environment
	return &Manager{
		stateDirectory: config.StateDirectory,
		nodeID:         config.NodeID,
		runner:         config.Runner,
		ready:          config.Ready,
		lookPath:       config.LookPath,
		now:            config.Now,
		random:         config.Random,
		environment:    func() []string { return workerEnvironment(parentEnvironment()) },
		consoles:       make(map[string]*consoleProcess),
	}, nil
}

func workerEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		if strings.HasPrefix(entry, "SNOWCAT_COCKPIT_MCP_TOKEN=") || strings.HasPrefix(entry, "SNOWCAT_COCKPIT_MCP_URL=") {
			continue
		}
		result = append(result, entry)
	}
	return result
}

func (manager *Manager) Launch(ctx context.Context, request LaunchRequest) (Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if err := validateRequest(&request); err != nil {
		return Record{}, err
	}
	if err := manager.ready(request.Provider); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	gitPath, err := manager.lookPath("git")
	if err != nil {
		return Record{}, fmt.Errorf("%w: git is not available", ErrNotReady)
	}
	tmuxPath, err := manager.lookPath("tmux")
	if err != nil {
		return Record{}, fmt.Errorf("%w: tmux is not available", ErrNotReady)
	}
	providerPath, err := manager.lookPath(request.Provider)
	if err != nil {
		return Record{}, fmt.Errorf("%w: %s is not available", ErrNotReady, request.Provider)
	}

	source, err := canonicalDirectory(request.Source)
	if err != nil {
		return Record{}, fmt.Errorf("%w: source: %v", ErrInvalid, err)
	}
	output, err := manager.run(ctx, gitPath, source, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return Record{}, fmt.Errorf("%w: source is not a Git working tree", ErrInvalid)
	}
	source, err = canonicalDirectory(strings.TrimSpace(string(output)))
	if err != nil {
		return Record{}, fmt.Errorf("%w: resolve Git source: %v", ErrInvalid, err)
	}
	output, err = manager.run(ctx, gitPath, source, nil, "rev-parse", "--verify", request.BaseRef+"^{commit}")
	if err != nil {
		return Record{}, fmt.Errorf("%w: base ref does not resolve to a local commit", ErrInvalid)
	}
	baseCommit := strings.TrimSpace(string(output))
	if !commitRE.MatchString(baseCommit) {
		return Record{}, fmt.Errorf("%w: Git returned an invalid base commit", ErrInvalid)
	}

	workerID, err := newWorkerID(manager.random)
	if err != nil {
		return Record{}, err
	}
	workspace := filepath.Join(manager.stateDirectory, "workspaces", workerID, "checkout")
	branch := "cockpit/" + workerID
	now := manager.now().UTC()
	record := Record{
		Version: recordVersion, ID: workerID, NodeID: manager.nodeID,
		Provider: request.Provider, Role: request.Role, Repository: request.Repository,
		Source: source, Workspace: workspace, BaseRef: request.BaseRef,
		BaseCommit: baseCommit, Branch: branch, Status: StatusAllocating,
		Detail: "allocating isolated Git worktree", CreatedAt: now,
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return Record{}, fmt.Errorf("create worker workspace root: %w", err)
	}
	if err := manager.write(record); err != nil {
		return Record{}, err
	}
	if _, err := manager.run(ctx, gitPath, source, nil, "worktree", "add", "-b", branch, workspace, baseCommit); err != nil {
		return manager.fail(record, "Git worktree allocation failed", err)
	}
	for _, skillRoot := range []string{
		filepath.Join(workspace, ".agents", "skills"),
		filepath.Join(workspace, ".claude", "skills"),
	} {
		if _, err := profile.InstallKit(skillRoot); err != nil {
			return manager.fail(record, "locked worker kit installation failed", err)
		}
	}
	excludePath, err := manager.writeExcludes(workerID)
	if err != nil {
		return manager.fail(record, "Git exclusion setup failed", err)
	}
	environment := gitEnvironment(manager.environment(), excludePath)
	prompt := BuildPrompt(workerID, request.Role, request.Repository)
	providerArguments := []string{prompt}
	if request.Provider == "copilot" {
		providerArguments = []string{"-i", prompt}
	}
	if err := manager.startTmux(ctx, tmuxPath, record, providerPath, providerArguments, environment); err != nil {
		return manager.fail(record, "tmux provider launch failed", err)
	}
	startedAt := manager.now().UTC()
	record.Status = StatusRunning
	record.Detail = "provider running; terminal and workspace retained"
	record.StartedAt = &startedAt
	if err := manager.write(record); err != nil {
		return record, err
	}
	return record, nil
}

func (manager *Manager) List(ctx context.Context) ([]Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	records, err := manager.readAll()
	if err != nil {
		return nil, err
	}
	for index := range records {
		records[index] = manager.reconcile(ctx, records[index])
	}
	sort.Slice(records, func(left, right int) bool {
		return records[left].CreatedAt.After(records[right].CreatedAt)
	})
	return records, nil
}

func (manager *Manager) Get(ctx context.Context, workerID string) (Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if !workerIDRE.MatchString(workerID) {
		return Record{}, fmt.Errorf("%w: invalid worker ID", ErrInvalid)
	}
	record, err := manager.read(workerID)
	if err != nil {
		return Record{}, err
	}
	return manager.reconcile(ctx, record), nil
}

func (manager *Manager) Stop(ctx context.Context, workerID string) (Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	record, err := manager.read(workerID)
	if err != nil {
		return Record{}, err
	}
	if record.Status == StatusCleaned {
		return Record{}, fmt.Errorf("%w: worker is already cleaned", ErrConflict)
	}
	if err := manager.stopTerminalLocked(ctx, record); err != nil {
		return Record{}, err
	}
	now := manager.now().UTC()
	record.Status = StatusStopped
	record.Detail = "terminal stopped; workspace retained"
	record.StoppedAt = &now
	if err := manager.write(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (manager *Manager) Cleanup(ctx context.Context, workerID string) (Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	record, err := manager.read(workerID)
	if err != nil {
		return Record{}, err
	}
	if record.Status == StatusCleaned {
		return record, nil
	}
	current := manager.reconcile(ctx, record)
	if current.Status == StatusRunning || current.Status == StatusAllocating {
		return Record{}, fmt.Errorf("%w: stop the worker before cleanup", ErrConflict)
	}
	gitPath, err := manager.lookPath("git")
	if err != nil {
		return Record{}, fmt.Errorf("cleanup requires git: %w", err)
	}
	if _, err := os.Stat(record.Workspace); err == nil {
		excludePath, err := manager.writeExcludes(record.ID)
		if err != nil {
			return Record{}, err
		}
		environment := gitEnvironment(manager.environment(), excludePath)
		output, err := manager.run(ctx, gitPath, record.Workspace, environment, "status", "--porcelain")
		if err != nil {
			return Record{}, fmt.Errorf("inspect workspace before cleanup: %w", err)
		}
		if len(bytes.TrimSpace(output)) != 0 {
			return Record{}, fmt.Errorf("%w: workspace has uncommitted or untracked changes", ErrConflict)
		}
		if err := manager.stopTerminalLocked(ctx, record); err != nil {
			return Record{}, err
		}
		if err := removeOwnedSkills(record.Workspace); err != nil {
			return Record{}, err
		}
		if _, err := manager.run(ctx, gitPath, record.Source, nil, "worktree", "remove", record.Workspace); err != nil {
			return Record{}, fmt.Errorf("remove clean Git worktree: %w", err)
		}
		_ = os.Remove(filepath.Dir(record.Workspace))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Record{}, fmt.Errorf("inspect workspace: %w", err)
	} else if err := manager.stopTerminalLocked(ctx, record); err != nil {
		return Record{}, err
	}
	now := manager.now().UTC()
	record.Status = StatusCleaned
	record.Detail = "workspace cleaned; branch retained"
	record.CleanedAt = &now
	if err := manager.write(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (manager *Manager) OpenConsole(ctx context.Context, workerID string) (Console, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	record, err := manager.read(workerID)
	if err != nil {
		return Console{}, err
	}
	if record.Status == StatusCleaned {
		return Console{}, fmt.Errorf("%w: worker terminal was cleaned", ErrConflict)
	}
	if existing := manager.consoles[workerID]; existing != nil && existing.command.ProcessState == nil {
		return Console{URL: existing.url}, nil
	}
	tmuxPath, err := manager.lookPath("tmux")
	if err != nil || !manager.tmuxExists(ctx, tmuxPath, record) {
		return Console{}, fmt.Errorf("%w: worker terminal is unavailable", ErrConflict)
	}
	ttydPath, err := manager.lookPath("ttyd")
	if err != nil {
		return Console{}, fmt.Errorf("%w: ttyd is not available", ErrNotReady)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return Console{}, fmt.Errorf("allocate loopback console port: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		return Console{}, fmt.Errorf("release loopback console port: %w", err)
	}
	interfaceName := "lo"
	if runtime.GOOS == "darwin" {
		interfaceName = "lo0"
	}
	if override := os.Getenv("SNOWCAT_COCKPIT_TTYD_INTERFACE"); override != "" {
		if !interfaceRE.MatchString(override) {
			return Console{}, fmt.Errorf("%w: invalid ttyd interface override", ErrInvalid)
		}
		interfaceName = override
	}
	command := exec.Command(ttydPath,
		"-i", interfaceName,
		"-p", strconv.Itoa(port),
		"-W", "-O", "-m", "1",
		tmuxPath, "-S", manager.socketPath(workerID), "attach-session", "-t", "worker",
	)
	command.Env = manager.environment()
	if err := command.Start(); err != nil {
		return Console{}, fmt.Errorf("start loopback worker console: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	process := &consoleProcess{command: command, url: url}
	manager.consoles[workerID] = process
	go func() {
		_ = command.Wait()
		manager.mutex.Lock()
		if manager.consoles[workerID] == process {
			delete(manager.consoles, workerID)
		}
		manager.mutex.Unlock()
	}()

	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		connection, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 50*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return Console{URL: url}, nil
		}
		if time.Now().After(deadline) {
			manager.stopConsoleLocked(workerID)
			return Console{}, errors.New("worker console did not become ready on loopback")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (manager *Manager) Close() error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	for workerID := range manager.consoles {
		manager.stopConsoleLocked(workerID)
	}
	return nil
}

func (manager *Manager) stopConsoleLocked(workerID string) {
	process := manager.consoles[workerID]
	if process == nil {
		return
	}
	delete(manager.consoles, workerID)
	if process.command.Process != nil && process.command.ProcessState == nil {
		_ = process.command.Process.Kill()
	}
}

func (manager *Manager) stopTerminalLocked(ctx context.Context, record Record) error {
	manager.stopConsoleLocked(record.ID)
	if tmuxPath, err := manager.lookPath("tmux"); err == nil && manager.tmuxExists(ctx, tmuxPath, record) {
		if _, err := manager.run(ctx, tmuxPath, "", nil, "-S", manager.socketPath(record.ID), "kill-server"); err != nil {
			return fmt.Errorf("stop retained worker terminal: %w", err)
		}
	}
	return nil
}

func (manager *Manager) AttachCommand(ctx context.Context, workerID string) (Command, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	record, err := manager.read(workerID)
	if err != nil {
		return Command{}, err
	}
	if record.Status == StatusCleaned {
		return Command{}, fmt.Errorf("%w: worker terminal was cleaned", ErrConflict)
	}
	tmuxPath, err := manager.lookPath("tmux")
	if err != nil || !manager.tmuxExists(ctx, tmuxPath, record) {
		return Command{}, fmt.Errorf("%w: worker terminal is unavailable", ErrConflict)
	}
	return Command{Name: tmuxPath, Arguments: []string{"-S", manager.socketPath(record.ID), "attach-session", "-t", "worker"}, Env: manager.environment()}, nil
}

func BuildPrompt(workerID, role, repository string) string {
	if role == "discoverer" {
		return fmt.Sprintf("Use the work-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. Work only kinds ending in -discovery for repository %s. Claim at most one item. Treat it as read-only discovery: do not edit files or open a GitHub artifact. Complete with concrete evidence and at most one bounded follow-up when justified. Every follow-up must declare requiredArtifact: use pull-request with write and open-pr for a change, or none for read-only work. Report the result to Snowcat, then stop.", workerID, repository)
	}
	if role == "reviewer" {
		return fmt.Sprintf("Use the review-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. Work only pr-review items for repository %s. Claim at most one item, report its structured verdict to Snowcat, then stop.", workerID, repository)
	}
	return fmt.Sprintf("Use the work-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. Work only kinds ending in -fix, plus exact pr-cure and pr-cure-change items, for repository %s. Claim at most one item. Before substantive work on any change item, require open-pr in allowedActions; if it is absent, release the item immediately as undeliverable and stop. Otherwise complete it within its allowed actions, deliver the required commit and pull-request artifacts, report the result to Snowcat, then stop.", workerID, repository)
}

func validateRequest(request *LaunchRequest) error {
	if request.BaseRef == "" {
		request.BaseRef = "HEAD"
	}
	if request.Provider != "codex" && request.Provider != "claude" && request.Provider != "copilot" {
		return fmt.Errorf("%w: unsupported provider", ErrInvalid)
	}
	if request.Role != "discoverer" && request.Role != "implementer" && request.Role != "reviewer" {
		return fmt.Errorf("%w: unsupported role", ErrInvalid)
	}
	if !repositoryRE.MatchString(request.Repository) {
		return fmt.Errorf("%w: repository must be owner/name", ErrInvalid)
	}
	if request.Source == "" || len(request.BaseRef) > 160 || strings.HasPrefix(request.BaseRef, "-") || strings.ContainsAny(request.BaseRef, "\x00\n\r") {
		return fmt.Errorf("%w: source and a safe local base ref are required", ErrInvalid)
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return resolved, nil
}

func newWorkerID(random io.Reader) (string, error) {
	value := make([]byte, 8)
	if _, err := io.ReadFull(random, value); err != nil {
		return "", fmt.Errorf("create worker ID: %w", err)
	}
	return "worker-" + hex.EncodeToString(value), nil
}

func (manager *Manager) startTmux(ctx context.Context, tmuxPath string, record Record, providerPath string, providerArguments, environment []string) error {
	if err := os.MkdirAll(manager.socketDirectory(), 0o700); err != nil {
		return fmt.Errorf("create private terminal directory: %w", err)
	}
	if err := os.Chmod(manager.socketDirectory(), 0o700); err != nil {
		return fmt.Errorf("secure private terminal directory: %w", err)
	}
	commandLine := shellJoin(append([]string{providerPath}, providerArguments...))
	arguments := []string{
		"-S", manager.socketPath(record.ID),
		"new-session", "-d", "-s", "worker", "-n", "console", "-c", record.Workspace,
		"while :; do sleep 3600; done",
		";", "set-window-option", "-t", "worker:console", "remain-on-exit", "on",
		";", "set-window-option", "-t", "worker:console", "allow-rename", "off",
		";", "respawn-pane", "-k", "-t", "worker:console", commandLine,
	}
	_, err := manager.run(ctx, tmuxPath, "", environment, arguments...)
	return err
}

func (manager *Manager) reconcile(ctx context.Context, record Record) Record {
	if record.Status != StatusRunning {
		return record
	}
	tmuxPath, err := manager.lookPath("tmux")
	if err != nil {
		record.Status = StatusFailed
		record.Detail = "tmux is unavailable; workspace retained"
		return record
	}
	output, err := manager.run(ctx, tmuxPath, "", nil, "-S", manager.socketPath(record.ID), "display-message", "-p", "-t", "worker:console", "#{pane_dead}")
	if err != nil {
		record.Status = StatusFailed
		record.Detail = "retained terminal is unavailable; workspace retained"
		return record
	}
	if strings.TrimSpace(string(output)) == "1" {
		record.Status = StatusExited
		record.Detail = "provider exited; terminal and workspace retained"
	}
	return record
}

func (manager *Manager) tmuxExists(ctx context.Context, tmuxPath string, record Record) bool {
	_, err := manager.run(ctx, tmuxPath, "", nil, "-S", manager.socketPath(record.ID), "has-session", "-t", "worker")
	return err == nil
}

func (manager *Manager) socketPath(workerID string) string {
	return filepath.Join(manager.socketDirectory(), strings.TrimPrefix(workerID, "worker-")+".sock")
}

func (manager *Manager) socketDirectory() string {
	digest := sha256.Sum256([]byte(manager.nodeID))
	return filepath.Join(os.TempDir(), "sc-"+hex.EncodeToString(digest[:8]))
}

func shellJoin(arguments []string) string {
	quoted := make([]string, len(arguments))
	for index, argument := range arguments {
		quoted[index] = "'" + strings.ReplaceAll(argument, "'", `'"'"'`) + "'"
	}
	return strings.Join(quoted, " ")
}

func gitEnvironment(environment []string, excludePath string) []string {
	result := append([]string(nil), environment...)
	result = setEnvironment(result, "GIT_CONFIG_COUNT", "1")
	result = setEnvironment(result, "GIT_CONFIG_KEY_0", "core.excludesFile")
	result = setEnvironment(result, "GIT_CONFIG_VALUE_0", excludePath)
	return result
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	for index := range environment {
		if strings.HasPrefix(environment[index], prefix) {
			environment[index] = prefix + value
			return environment
		}
	}
	return append(environment, prefix+value)
}

func (manager *Manager) run(ctx context.Context, name, directory string, environment []string, arguments ...string) ([]byte, error) {
	if environment == nil {
		environment = manager.environment()
	}
	return manager.runner.Run(ctx, Command{Name: name, Arguments: arguments, Directory: directory, Env: environment})
}

func (manager *Manager) fail(record Record, detail string, cause error) (Record, error) {
	record.Status = StatusFailed
	record.Detail = detail + "; workspace retained for explicit cleanup"
	if err := manager.write(record); err != nil {
		return record, errors.Join(cause, err)
	}
	return record, fmt.Errorf("%s: %w", detail, cause)
}

func (manager *Manager) recordsDirectory() string {
	return filepath.Join(manager.stateDirectory, "workers")
}

func (manager *Manager) recordPath(workerID string) string {
	return filepath.Join(manager.recordsDirectory(), workerID+".json")
}

func (manager *Manager) excludePath(workerID string) string {
	return filepath.Join(manager.recordsDirectory(), workerID+".git-excludes")
}

func (manager *Manager) writeExcludes(workerID string) (string, error) {
	if err := os.MkdirAll(manager.recordsDirectory(), 0o700); err != nil {
		return "", fmt.Errorf("create worker state directory: %w", err)
	}
	path := manager.excludePath(workerID)
	if err := os.WriteFile(path, []byte("/.agents/\n/.claude/\n"), 0o600); err != nil {
		return "", fmt.Errorf("write worker Git exclusions: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("secure worker Git exclusions: %w", err)
	}
	return path, nil
}

func (manager *Manager) write(record Record) error {
	if err := os.MkdirAll(manager.recordsDirectory(), 0o700); err != nil {
		return fmt.Errorf("create worker state directory: %w", err)
	}
	if err := os.Chmod(manager.recordsDirectory(), 0o700); err != nil {
		return fmt.Errorf("secure worker state directory: %w", err)
	}
	temporary, err := os.CreateTemp(manager.recordsDirectory(), ".worker-*.json")
	if err != nil {
		return fmt.Errorf("create temporary worker record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary worker record: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode worker record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync worker record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close worker record: %w", err)
	}
	if err := os.Rename(temporaryPath, manager.recordPath(record.ID)); err != nil {
		return fmt.Errorf("install worker record: %w", err)
	}
	return nil
}

func (manager *Manager) read(workerID string) (Record, error) {
	if !workerIDRE.MatchString(workerID) {
		return Record{}, fmt.Errorf("%w: invalid worker ID", ErrInvalid)
	}
	file, err := os.Open(manager.recordPath(workerID))
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, fmt.Errorf("read worker record: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxOutput))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, fmt.Errorf("decode worker record: %w", err)
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (manager *Manager) readAll() ([]Record, error) {
	entries, err := os.ReadDir(manager.recordsDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worker records: %w", err)
	}
	records := make([]Record, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		workerID := strings.TrimSuffix(entry.Name(), ".json")
		record, err := manager.read(workerID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func validateRecord(record Record) error {
	if record.Version != recordVersion || !workerIDRE.MatchString(record.ID) || record.NodeID == "" {
		return errors.New("decode worker record: invalid identity or version")
	}
	if record.Provider != "codex" && record.Provider != "claude" && record.Provider != "copilot" {
		return errors.New("decode worker record: invalid provider")
	}
	if record.Role != "discoverer" && record.Role != "implementer" && record.Role != "reviewer" {
		return errors.New("decode worker record: invalid role")
	}
	if record.CreatedAt.IsZero() || record.Workspace == "" || record.Source == "" {
		return errors.New("decode worker record: incomplete lifecycle")
	}
	return nil
}

func removeOwnedSkills(workspace string) error {
	manifest := profile.LockedManifest()
	for _, providerRoot := range []string{".agents", ".claude"} {
		for _, skill := range manifest.Skills {
			path := filepath.Join(workspace, providerRoot, "skills", skill.Name, "SKILL.md")
			content, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect Cockpit-owned skill before cleanup: %w", err)
			}
			digest := sha256.Sum256(content)
			if hex.EncodeToString(digest[:]) != skill.SHA256 {
				return fmt.Errorf("%w: Cockpit-owned skill path drifted", ErrConflict)
			}
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove Cockpit-owned skill: %w", err)
			}
			_ = os.Remove(filepath.Dir(path))
		}
		_ = os.Remove(filepath.Join(workspace, providerRoot, "skills"))
		_ = os.Remove(filepath.Join(workspace, providerRoot))
	}
	return nil
}

type limitedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (buffer *limitedBuffer) Write(content []byte) (int, error) {
	original := len(content)
	remaining := maxOutput - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return original, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.overflow = true
	}
	_, _ = buffer.Buffer.Write(content)
	return original, nil
}
