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
	"net/url"
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
	AdapterHost     = "host"
	AdapterOCI      = "oci"
	RuntimePodman   = "podman"
	RuntimeDocker   = "docker"
	PostureRootless = "rootless"
	PostureRootful  = "rootful"
	OCIModelWork    = "gpt-5.6-sol"
	OCIModelReview  = "gpt-5.6-terra"
	OCIModelAuto    = "auto"
	OCIModelSonnet  = "sonnet"
	OCIModelOpus    = "opus"

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
	imageIDRE    = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._/:@-]*@)?sha256:[0-9a-f]{64}$`)
	scpRemoteRE  = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^[:space:]]+$`)
	dockerHostRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$`)
)

type LaunchRequest struct {
	Adapter    string `json:"adapter,omitempty"`
	Runtime    string `json:"runtime,omitempty"`
	Provider   string `json:"provider"`
	Role       string `json:"role"`
	Repository string `json:"repository"`
	Source     string `json:"source"`
	BaseRef    string `json:"baseRef,omitempty"`
}

type Record struct {
	Version        int        `json:"version"`
	ID             string     `json:"id"`
	NodeID         string     `json:"nodeId"`
	Adapter        string     `json:"adapter"`
	Runtime        string     `json:"runtime,omitempty"`
	RuntimePosture string     `json:"runtimePosture,omitempty"`
	Provider       string     `json:"provider"`
	Model          string     `json:"model,omitempty"`
	Role           string     `json:"role"`
	Repository     string     `json:"repository"`
	Source         string     `json:"source"`
	Workspace      string     `json:"workspace"`
	BaseRef        string     `json:"baseRef"`
	BaseCommit     string     `json:"baseCommit"`
	Branch         string     `json:"branch"`
	Status         string     `json:"status"`
	Detail         string     `json:"detail"`
	CreatedAt      time.Time  `json:"createdAt"`
	StartedAt      *time.Time `json:"startedAt,omitempty"`
	StoppedAt      *time.Time `json:"stoppedAt,omitempty"`
	CleanedAt      *time.Time `json:"cleanedAt,omitempty"`
}

type BaseInspection struct {
	Source     string `json:"source"`
	BaseRef    string `json:"baseRef"`
	BaseCommit string `json:"baseCommit"`
	Upstream   string `json:"upstream,omitempty"`
	Status     string `json:"status"`
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Detail     string `json:"detail"`
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
	OCI            OCIConfig
}

type OCIConfig struct {
	Images        map[string]string
	DockerImages  map[string]string
	DockerAddHost string
	CodexHome     string
	ClaudeHome    string
	CopilotHome   string
	GHConfigDir   string
}

type ociRuntimeSelection struct {
	Path    string
	Image   string
	Posture string
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
	oci            OCIConfig
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
		oci:            config.OCI,
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
	var providerPath string
	var runtimeSelection ociRuntimeSelection
	if request.Adapter == AdapterHost {
		providerPath, err = manager.lookPath(request.Provider)
		if err != nil {
			return Record{}, fmt.Errorf("%w: %s is not available", ErrNotReady, request.Provider)
		}
	} else {
		runtimeSelection, err = manager.validateOCI(ctx, request)
		if err != nil {
			return Record{}, err
		}
	}

	base, err := manager.inspectBase(ctx, gitPath, request.Source, request.BaseRef)
	if err != nil {
		return Record{}, err
	}
	source := base.Source
	baseCommit := base.BaseCommit
	var output []byte
	var remoteURL string
	if request.Adapter == AdapterOCI {
		output, err = manager.run(ctx, gitPath, source, nil, "remote", "get-url", "--push", "origin")
		if err != nil {
			return Record{}, fmt.Errorf("%w: OCI source requires an origin push URL", ErrInvalid)
		}
		remoteURL, err = projectGitHubRemote(strings.TrimSpace(string(output)), request.Repository)
		if err != nil {
			return Record{}, fmt.Errorf("%w: OCI source origin: %v", ErrInvalid, err)
		}
	}

	workerID, err := newWorkerID(manager.random)
	if err != nil {
		return Record{}, err
	}
	workspace := filepath.Join(manager.stateDirectory, "workspaces", workerID, "checkout")
	branch := "cockpit/" + workerID
	model := ""
	if request.Adapter == AdapterOCI {
		model = ociModel(request.Provider, request.Role)
	}
	now := manager.now().UTC()
	record := Record{
		Version: recordVersion, ID: workerID, NodeID: manager.nodeID,
		Adapter: request.Adapter, Runtime: request.Runtime, RuntimePosture: runtimeSelection.Posture,
		Provider: request.Provider, Model: model, Role: request.Role, Repository: request.Repository,
		Source: source, Workspace: workspace, BaseRef: request.BaseRef,
		BaseCommit: baseCommit, Branch: branch, Status: StatusAllocating,
		Detail: "allocating isolated Git workspace", CreatedAt: now,
	}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return Record{}, fmt.Errorf("create worker workspace root: %w", err)
	}
	if err := manager.write(record); err != nil {
		return Record{}, err
	}
	if request.Adapter == AdapterOCI {
		if _, err := manager.run(ctx, gitPath, "", nil, "clone", "--local", "--no-hardlinks", "--no-checkout", source, workspace); err != nil {
			return manager.fail(record, "self-contained Git workspace allocation failed", err)
		}
		if _, err := manager.run(ctx, gitPath, workspace, nil, "remote", "set-url", "origin", remoteURL); err != nil {
			return manager.fail(record, "Git origin projection failed", err)
		}
		if _, err := manager.run(ctx, gitPath, workspace, nil, "checkout", "-b", branch, baseCommit); err != nil {
			return manager.fail(record, "Git branch checkout failed", err)
		}
	} else if _, err := manager.run(ctx, gitPath, source, nil, "worktree", "add", "-b", branch, workspace, baseCommit); err != nil {
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
	if request.Adapter == AdapterOCI {
		if err := writeOCIExcludes(workspace); err != nil {
			return manager.fail(record, "OCI Git exclusion setup failed", err)
		}
	}
	environment := gitEnvironment(manager.environment(), excludePath)
	prompt := BuildPrompt(workerID, request.Role, request.Repository)
	launchCommand := []string{providerPath, prompt}
	if request.Adapter == AdapterOCI {
		launchCommand = append([]string{runtimeSelection.Path}, manager.ociArguments(record, runtimeSelection.Image, prompt)...)
		environment = ociHostEnvironment(environment)
	} else if request.Provider == "copilot" {
		launchCommand = []string{providerPath, "-i", prompt}
	}
	if err := manager.startTmux(ctx, tmuxPath, record, launchCommand, environment); err != nil {
		return manager.fail(record, "tmux provider launch failed", err)
	}
	startedAt := manager.now().UTC()
	record.Status = StatusRunning
	record.Detail = request.Adapter + " provider running; terminal and workspace retained"
	if request.Adapter == AdapterOCI {
		record.Detail = fmt.Sprintf("oci %s (%s) provider running; terminal and workspace retained", request.Runtime, runtimeSelection.Posture)
	}
	record.StartedAt = &startedAt
	if err := manager.write(record); err != nil {
		return record, err
	}
	return record, nil
}

func (manager *Manager) InspectBase(ctx context.Context, source, baseRef string) (BaseInspection, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	if err := validateBaseInput(source, baseRef); err != nil {
		return BaseInspection{}, err
	}
	gitPath, err := manager.lookPath("git")
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: git is not available", ErrNotReady)
	}
	return manager.inspectBase(ctx, gitPath, source, baseRef)
}

func validateBaseInput(source, baseRef string) error {
	if source == "" || baseRef == "" || len(baseRef) > 160 || strings.HasPrefix(baseRef, "-") || strings.ContainsAny(baseRef, "\x00\n\r") {
		return fmt.Errorf("%w: source and a safe local base ref are required", ErrInvalid)
	}
	return nil
}

func (manager *Manager) inspectBase(ctx context.Context, gitPath, source, baseRef string) (BaseInspection, error) {
	inspection := BaseInspection{BaseRef: baseRef, Status: "untracked"}
	resolved, err := canonicalDirectory(source)
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: source: %v", ErrInvalid, err)
	}
	output, err := manager.run(ctx, gitPath, resolved, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: source is not a Git working tree", ErrInvalid)
	}
	resolved, err = canonicalDirectory(strings.TrimSpace(string(output)))
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: resolve Git source: %v", ErrInvalid, err)
	}
	inspection.Source = resolved
	output, err = manager.run(ctx, gitPath, resolved, nil, "rev-parse", "--verify", baseRef+"^{commit}")
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: base ref does not resolve to a local commit", ErrInvalid)
	}
	inspection.BaseCommit = strings.TrimSpace(string(output))
	if !commitRE.MatchString(inspection.BaseCommit) {
		return BaseInspection{}, fmt.Errorf("%w: Git returned an invalid base commit", ErrInvalid)
	}
	output, err = manager.run(ctx, gitPath, resolved, nil, "rev-parse", "--abbrev-ref", "--symbolic-full-name", baseRef+"@{upstream}")
	if err != nil {
		inspection.Detail = fmt.Sprintf("%s resolves locally to %.12s; no local upstream relation is configured", baseRef, inspection.BaseCommit)
		return inspection, nil
	}
	inspection.Upstream = strings.TrimSpace(string(output))
	if inspection.Upstream == "" || strings.ContainsAny(inspection.Upstream, "\x00\n\r") {
		return BaseInspection{}, fmt.Errorf("%w: Git returned an invalid upstream ref", ErrInvalid)
	}
	output, err = manager.run(ctx, gitPath, resolved, nil, "rev-list", "--left-right", "--count", baseRef+"..."+inspection.Upstream)
	if err != nil {
		return BaseInspection{}, fmt.Errorf("%w: inspect local base relation", ErrInvalid)
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(string(output)), "%d %d", &inspection.Ahead, &inspection.Behind); err != nil || inspection.Ahead < 0 || inspection.Behind < 0 {
		return BaseInspection{}, fmt.Errorf("%w: Git returned invalid ahead/behind counts", ErrInvalid)
	}
	switch {
	case inspection.Ahead > 0 && inspection.Behind > 0:
		inspection.Status = "diverged"
	case inspection.Behind > 0:
		inspection.Status = "behind"
	case inspection.Ahead > 0:
		inspection.Status = "ahead"
	default:
		inspection.Status = "current"
	}
	inspection.Detail = fmt.Sprintf("%s is %d ahead and %d behind local upstream %s; Cockpit did not fetch", baseRef, inspection.Ahead, inspection.Behind, inspection.Upstream)
	return inspection, nil
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
		if record.Adapter == AdapterOCI {
			if record.Workspace != manager.workspacePath(record.ID) {
				return Record{}, fmt.Errorf("%w: OCI workspace path does not match worker identity", ErrConflict)
			}
			refspec := "refs/heads/" + record.Branch + ":refs/heads/" + record.Branch
			if _, err := manager.run(ctx, gitPath, record.Source, nil, "fetch", "--no-tags", record.Workspace, refspec); err != nil {
				return Record{}, fmt.Errorf("retain OCI worker branch in source: %w", err)
			}
			if err := os.RemoveAll(record.Workspace); err != nil {
				return Record{}, fmt.Errorf("remove clean OCI workspace: %w", err)
			}
		} else if _, err := manager.run(ctx, gitPath, record.Source, nil, "worktree", "remove", record.Workspace); err != nil {
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
	if record.Adapter == AdapterOCI {
		runtimeName := record.Runtime
		if runtimeName == "" {
			runtimeName = RuntimePodman
		}
		runtimePath, err := manager.lookPath(runtimeName)
		if err != nil {
			return fmt.Errorf("stop OCI worker: %s is unavailable: %w", runtimeName, err)
		}
		arguments := []string{"stop", "--time", "10", manager.containerName(record.ID)}
		if runtimeName == RuntimePodman {
			arguments = []string{"stop", "--ignore", "--time", "10", manager.containerName(record.ID)}
		}
		output, stopErr := manager.run(ctx, runtimePath, "", ociHostEnvironment(manager.environment()), arguments...)
		missingDockerContainer := runtimeName == RuntimeDocker && strings.Contains(strings.ToLower(string(output)), "no such container")
		if stopErr != nil && !missingDockerContainer {
			return fmt.Errorf("stop OCI worker container: %w", stopErr)
		}
	}
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
	return fmt.Sprintf("Use the work-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. For repository %s, list queued work once, derive the exact observed kind set excluding kinds ending in -discovery, exact pr-review, and exact release-needed, and claim at most one item with only that set. Do not use a fixed implementation-kind whitelist; implementation, issue-resolution, pr-review-fix, cures, fixes, and future worker kinds are eligible. release-needed remains human-operated. The workspace is already isolated on a Cockpit-owned branch: do not create, rename, or switch branches. Before substantive work on any change item, require both open-pr in allowedActions and requiredArtifact pull-request; if either is absent, release the item immediately as undeliverable and stop. When both are present, they are explicit operator authorization to commit, push the current branch, and open the required draft pull request without asking for further permission. Complete the item within its allowed actions, report the commit and pull-request artifacts to Snowcat, then stop.", workerID, repository)
}

func validateRequest(request *LaunchRequest) error {
	if request.Adapter == "" {
		request.Adapter = AdapterHost
	}
	if request.Adapter == AdapterOCI && request.Runtime == "" {
		request.Runtime = RuntimePodman
	}
	if request.BaseRef == "" {
		request.BaseRef = "HEAD"
	}
	if request.Adapter != AdapterHost && request.Adapter != AdapterOCI {
		return fmt.Errorf("%w: adapter must be host or oci", ErrInvalid)
	}
	if request.Adapter == AdapterHost && request.Runtime != "" {
		return fmt.Errorf("%w: runtime is valid only with the oci adapter", ErrInvalid)
	}
	if request.Adapter == AdapterOCI && request.Runtime != RuntimePodman && request.Runtime != RuntimeDocker {
		return fmt.Errorf("%w: OCI runtime must be podman or docker", ErrInvalid)
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
	return validateBaseInput(request.Source, request.BaseRef)
}

func (manager *Manager) validateOCI(ctx context.Context, request LaunchRequest) (ociRuntimeSelection, error) {
	if request.Provider != "codex" && request.Provider != "claude" && request.Provider != "copilot" {
		return ociRuntimeSelection{}, fmt.Errorf("%w: OCI supports only codex, claude, and copilot", ErrNotReady)
	}
	if runtime.GOOS != "linux" {
		return ociRuntimeSelection{}, fmt.Errorf("%w: the OCI adapter requires Linux containers", ErrNotReady)
	}
	image := manager.oci.Images[request.Provider]
	if request.Runtime == RuntimeDocker {
		image = manager.oci.DockerImages[request.Provider]
	}
	if !imageIDRE.MatchString(image) {
		return ociRuntimeSelection{}, fmt.Errorf("%w: the %s %s image must be pinned by SHA-256", ErrNotReady, request.Runtime, request.Provider)
	}
	runtimePath, err := manager.lookPath(request.Runtime)
	if err != nil {
		return ociRuntimeSelection{}, fmt.Errorf("%w: %s is not available", ErrNotReady, request.Runtime)
	}
	hostEnvironment := ociHostEnvironment(manager.environment())
	selection := ociRuntimeSelection{Path: runtimePath, Image: image}
	if request.Runtime == RuntimePodman {
		output, infoErr := manager.run(ctx, runtimePath, "", hostEnvironment, "info", "--format", "{{.Host.Security.Rootless}}")
		if infoErr != nil || strings.TrimSpace(string(output)) != "true" {
			return ociRuntimeSelection{}, fmt.Errorf("%w: Podman is not structurally rootless", ErrNotReady)
		}
		if _, imageErr := manager.run(ctx, runtimePath, "", hostEnvironment, "image", "exists", image); imageErr != nil {
			return ociRuntimeSelection{}, fmt.Errorf("%w: pinned Podman worker image is not available locally", ErrNotReady)
		}
		selection.Posture = PostureRootless
	} else {
		if manager.oci.DockerAddHost != "" {
			if err := validateDockerAddHost(manager.oci.DockerAddHost); err != nil {
				return ociRuntimeSelection{}, fmt.Errorf("%w: Docker host mapping: %v", ErrNotReady, err)
			}
		}
		output, infoErr := manager.run(ctx, runtimePath, "", hostEnvironment, "info", "--format", "{{.OSType}}\n{{json .SecurityOptions}}")
		if infoErr != nil {
			return ociRuntimeSelection{}, fmt.Errorf("%w: Docker daemon is unavailable", ErrNotReady)
		}
		lines := strings.SplitN(strings.TrimSpace(string(output)), "\n", 2)
		if len(lines) == 0 || strings.TrimSpace(lines[0]) != "linux" {
			return ociRuntimeSelection{}, fmt.Errorf("%w: Docker is not serving Linux containers", ErrNotReady)
		}
		selection.Posture = PostureRootful
		if len(lines) == 2 && strings.Contains(strings.ToLower(lines[1]), "rootless") {
			selection.Posture = PostureRootless
		}
		if _, imageErr := manager.run(ctx, runtimePath, "", hostEnvironment, "image", "inspect", image); imageErr != nil {
			return ociRuntimeSelection{}, fmt.Errorf("%w: pinned Docker worker image is not available locally", ErrNotReady)
		}
	}
	inputs := []string{
		filepath.Join(manager.oci.GHConfigDir, "hosts.yml"),
		filepath.Join(manager.oci.GHConfigDir, "config.yml"),
	}
	switch request.Provider {
	case "codex":
		inputs = append(inputs,
			filepath.Join(manager.oci.CodexHome, "auth.json"),
			filepath.Join(manager.oci.CodexHome, "config.toml"),
		)
	case "copilot":
		inputs = append(inputs, filepath.Join(manager.oci.CopilotHome, "mcp-config.json"))
	case "claude":
		inputs = append(inputs, filepath.Join(manager.oci.ClaudeHome, ".credentials.json"))
	}
	for _, input := range inputs {
		if err := validatePrivateInput(input); err != nil {
			return ociRuntimeSelection{}, fmt.Errorf("%w: OCI input %s is unavailable: %v", ErrNotReady, filepath.Base(input), err)
		}
	}
	if !hasNonemptyEnvironment(manager.environment(), "SNOWCAT_MCP_TOKEN") {
		return ociRuntimeSelection{}, fmt.Errorf("%w: SNOWCAT_MCP_TOKEN is not present in the node environment", ErrNotReady)
	}
	if !hasNonemptyEnvironment(manager.environment(), "GH_TOKEN") {
		return ociRuntimeSelection{}, fmt.Errorf("%w: GH_TOKEN is not present in the node environment", ErrNotReady)
	}
	if request.Provider == "claude" && !hasNonemptyEnvironment(manager.environment(), "SNOWCAT_MCP_URL") {
		return ociRuntimeSelection{}, fmt.Errorf("%w: SNOWCAT_MCP_URL is not present in the node environment", ErrNotReady)
	}
	return selection, nil
}

func validateDockerAddHost(value string) error {
	host, address, found := strings.Cut(value, ":")
	if !found || len(host) > 253 || strings.Contains(address, ":") || !dockerHostRE.MatchString(host) || strings.Contains(host, "..") {
		return errors.New("must be one hostname and IPv4 address")
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return errors.New("must be one hostname and IPv4 address")
	}
	return nil
}

func validatePrivateInput(path string) error {
	if path == "" {
		return errors.New("path is not configured")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("must be a regular non-symlink file")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("group and other permission bits must be zero")
	}
	if !privateInputOwnedByCurrentUser(info) {
		return errors.New("must be owned by the current user")
	}
	return nil
}

func projectGitHubRemote(remote, repository string) (string, error) {
	if remote == "" || strings.ContainsAny(remote, "\x00\n\r") || strings.HasPrefix(remote, "-") {
		return "", errors.New("must be a safe non-empty URL")
	}
	if scpRemoteRE.MatchString(remote) {
		userHost, path, _ := strings.Cut(remote, ":")
		if userHost != "git@github.com" || strings.TrimSuffix(path, ".git") != repository {
			return "", errors.New("must identify the selected repository on github.com")
		}
		return "https://github.com/" + repository + ".git", nil
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Host == "" {
		return "", errors.New("must be a GitHub HTTPS or SSH URL")
	}
	if !strings.EqualFold(parsed.Hostname(), "github.com") || parsed.RawQuery != "" || parsed.Fragment != "" || strings.TrimSuffix(strings.TrimPrefix(parsed.Path, "/"), ".git") != repository {
		return "", errors.New("must identify the selected repository on github.com")
	}
	switch parsed.Scheme {
	case "https":
		if parsed.User != nil {
			return "", errors.New("HTTPS URL must not contain user information")
		}
	case "ssh":
		if parsed.User == nil || parsed.User.Username() != "git" {
			return "", errors.New("SSH URL must use the git user")
		}
		if _, present := parsed.User.Password(); present {
			return "", errors.New("SSH URL must not contain a password")
		}
	default:
		return "", errors.New("must use HTTPS or SSH")
	}
	return "https://github.com/" + repository + ".git", nil
}

func hasNonemptyEnvironment(environment []string, name string) bool {
	prefix := name + "="
	for _, entry := range environment {
		if strings.HasPrefix(entry, prefix) && len(entry) > len(prefix) {
			return true
		}
	}
	return false
}

func ociHostEnvironment(environment []string) []string {
	allowed := map[string]bool{
		"PATH": true, "HOME": true, "USER": true, "LOGNAME": true,
		"XDG_RUNTIME_DIR": true, "XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true,
		"XDG_CACHE_HOME": true, "DBUS_SESSION_BUS_ADDRESS": true,
		"CONTAINER_HOST": true, "TMPDIR": true,
		"SNOWCAT_MCP_TOKEN": true, "SNOWCAT_MCP_URL": true, "GH_TOKEN": true,
	}
	result := make([]string, 0, len(allowed))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && allowed[name] {
			result = append(result, entry)
		}
	}
	return result
}

func (manager *Manager) ociArguments(record Record, image, prompt string) []string {
	inputMount := func(source, destination string) string {
		return "type=bind,source=" + source + ",destination=" + destination + ",readonly"
	}
	arguments := []string{
		"run", "--rm", "--pull=never", "--tty",
		"--name", manager.containerName(record.ID),
		"--read-only",
	}
	if record.Runtime == RuntimePodman {
		arguments = append(arguments,
			"--read-only-tmpfs=false",
			"--userns=keep-id:uid=1000,gid=1000",
			"--user=1000:1000",
			"--cap-drop=ALL", "--security-opt=no-new-privileges",
		)
	} else {
		arguments = append(arguments,
			"--user=1000:1000",
			"--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		)
		if manager.oci.DockerAddHost != "" {
			arguments = append(arguments, "--add-host", manager.oci.DockerAddHost)
		}
	}
	arguments = append(arguments,
		"--pids-limit=512", "--log-driver=none",
		"--tmpfs=/home/cockpit:rw,size=2g,mode=1777",
		"--tmpfs=/tmp:rw,size=2g,mode=1777",
		"--tmpfs=/var/lib:rw,size=512m,mode=1777",
		"--mount", "type=bind,source="+record.Workspace+",destination=/workspace",
		"--mount", inputMount(filepath.Join(manager.oci.GHConfigDir, "hosts.yml"), "/run/cockpit/input/gh/hosts.yml"),
		"--mount", inputMount(filepath.Join(manager.oci.GHConfigDir, "config.yml"), "/run/cockpit/input/gh/config.yml"),
	)
	switch record.Provider {
	case "codex":
		arguments = append(arguments,
			"--mount", inputMount(filepath.Join(manager.oci.CodexHome, "auth.json"), "/run/cockpit/input/codex/auth.json"),
			"--mount", inputMount(filepath.Join(manager.oci.CodexHome, "config.toml"), "/run/cockpit/input/codex/config.toml"),
		)
	case "copilot":
		arguments = append(arguments,
			"--tmpfs=/home/cockpit/.cache/copilot:rw,exec,size=512m,mode=1777",
			"--mount", inputMount(filepath.Join(manager.oci.CopilotHome, "mcp-config.json"), "/run/cockpit/input/copilot/mcp-config.json"),
		)
	case "claude":
		arguments = append(arguments,
			"--mount", inputMount(filepath.Join(manager.oci.ClaudeHome, ".credentials.json"), "/run/cockpit/input/claude/.credentials.json"),
		)
	}
	arguments = append(arguments,
		"--env", "SNOWCAT_MCP_TOKEN",
		"--env", "GH_TOKEN",
	)
	if record.Provider == "claude" {
		arguments = append(arguments, "--env", "SNOWCAT_MCP_URL")
	}
	return append(arguments, image, prompt, record.Model)
}

func ociModel(provider, role string) string {
	if provider == "copilot" {
		return OCIModelAuto
	}
	if provider == "claude" {
		if role == "reviewer" {
			return OCIModelOpus
		}
		return OCIModelSonnet
	}
	if role == "reviewer" {
		return OCIModelReview
	}
	return OCIModelWork
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

func (manager *Manager) startTmux(ctx context.Context, tmuxPath string, record Record, launchCommand, environment []string) error {
	if err := os.MkdirAll(manager.socketDirectory(), 0o700); err != nil {
		return fmt.Errorf("create private terminal directory: %w", err)
	}
	if err := os.Chmod(manager.socketDirectory(), 0o700); err != nil {
		return fmt.Errorf("secure private terminal directory: %w", err)
	}
	commandLine := shellJoin(launchCommand)
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

func (manager *Manager) containerName(workerID string) string {
	return "cockpit-" + workerID
}

func (manager *Manager) workspacePath(workerID string) string {
	return filepath.Join(manager.stateDirectory, "workspaces", workerID, "checkout")
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

func writeOCIExcludes(workspace string) error {
	path := filepath.Join(workspace, ".git", "info", "exclude")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open private Git exclusions: %w", err)
	}
	defer file.Close()
	if _, err := io.WriteString(file, "\n/.agents/\n/.claude/\n"); err != nil {
		return fmt.Errorf("write private Git exclusions: %w", err)
	}
	return nil
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
	if record.Adapter == "" {
		record.Adapter = AdapterHost
	}
	if record.Adapter == AdapterOCI && record.Runtime == "" {
		record.Runtime = RuntimePodman
		record.RuntimePosture = PostureRootless
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
	if record.Adapter != AdapterHost && record.Adapter != AdapterOCI {
		return errors.New("decode worker record: invalid execution adapter")
	}
	if record.Adapter == AdapterHost && (record.Runtime != "" || record.RuntimePosture != "") {
		return errors.New("decode worker record: host worker has OCI runtime metadata")
	}
	if record.Adapter == AdapterOCI {
		if record.Runtime != RuntimePodman && record.Runtime != RuntimeDocker {
			return errors.New("decode worker record: invalid OCI runtime")
		}
		if record.RuntimePosture != PostureRootless && record.RuntimePosture != PostureRootful {
			return errors.New("decode worker record: invalid OCI runtime posture")
		}
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
