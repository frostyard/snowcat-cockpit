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
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
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

	StatusAllocating     = "allocating"
	StatusRunning        = "running"
	StatusExited         = "exited"
	StatusFailed         = "failed"
	StatusStopped        = "stopped"
	StatusCleaned        = "cleaned"
	KitOwnershipCockpit  = "cockpit"
	KitOwnershipCheckout = "checkout"

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
	// PinnedImageRE is the immutable worker image constraint (oci-workers spec):
	// a bare sha256 image ID or a reference suffixed by @sha256:<64 hex>.
	PinnedImageRE = imageIDRE
	scpRemoteRE   = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[^[:space:]]+$`)
	dockerHostRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]*[A-Za-z0-9]$`)
	mcpServerRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type LaunchRequest struct {
	Adapter    string `json:"adapter,omitempty"`
	Runtime    string `json:"runtime,omitempty"`
	Provider   string `json:"provider"`
	MCPServer  string `json:"mcpServer,omitempty"`
	Role       string `json:"role"`
	Repository string `json:"repository"`
	Source     string `json:"source"`
	BaseRef    string `json:"baseRef,omitempty"`
}

type Record struct {
	Version        int           `json:"version"`
	ID             string        `json:"id"`
	NodeID         string        `json:"nodeId"`
	Adapter        string        `json:"adapter"`
	Runtime        string        `json:"runtime,omitempty"`
	RuntimePosture string        `json:"runtimePosture,omitempty"`
	Provider       string        `json:"provider"`
	MCPServer      string        `json:"mcpServer,omitempty"`
	Model          string        `json:"model,omitempty"`
	Role           string        `json:"role"`
	Repository     string        `json:"repository"`
	Source         string        `json:"source"`
	Workspace      string        `json:"workspace"`
	BaseRef        string        `json:"baseRef"`
	BaseCommit     string        `json:"baseCommit"`
	Branch         string        `json:"branch"`
	ItemID         string        `json:"itemId,omitempty"`
	WorkKind       string        `json:"workKind,omitempty"`
	PullRequestURL string        `json:"pullRequestUrl,omitempty"`
	TargetRepo     string        `json:"targetRepository,omitempty"`
	TargetBranch   string        `json:"targetBranch,omitempty"`
	TargetHead     string        `json:"targetHead,omitempty"`
	TargetMode     string        `json:"targetMode,omitempty"`
	TargetedAt     *time.Time    `json:"targetedAt,omitempty"`
	Provisioning   *Provisioning `json:"provisioning,omitempty"`
	Kit            *KitRecord    `json:"kit,omitempty"`
	Status         string        `json:"status"`
	Detail         string        `json:"detail"`
	CreatedAt      time.Time     `json:"createdAt"`
	StartedAt      *time.Time    `json:"startedAt,omitempty"`
	StoppedAt      *time.Time    `json:"stoppedAt,omitempty"`
	CleanedAt      *time.Time    `json:"cleanedAt,omitempty"`
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

// KitRecord is the locked worker kit a worker was launched with: the Snowcat
// source revision and each Cockpit-owned skill's digest. Cleanup compares the
// retained skill files against these, not against whatever kit the node
// carries at cleanup time, so a re-vendored kit never makes an older
// workspace look tampered with.
type KitRecord struct {
	Revision  string            `json:"revision"`
	Skills    map[string]string `json:"skills"`
	Ownership string            `json:"ownership,omitempty"`
}

// CleanupOptions bounds what Cleanup may discard beyond a clean workspace.
type CleanupOptions struct {
	// DiscardDriftedSkills lets cleanup remove a Cockpit-owned skill file whose
	// digest matches neither the worker's recorded kit nor the current lock.
	// The branch is retained first either way; only an uncommitted edit to a
	// Cockpit-owned, git-excluded skill file is lost, and the record says so.
	DiscardDriftedSkills bool
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
	StateDirectory  string
	SkillsDirectory string
	NodeID          string
	TargetHelper    string
	Runner          Runner
	Ready           func(string) error
	ReadyMCP        func(string, string) error
	LookPath        func(string) (string, error)
	Now             func() time.Time
	Random          io.Reader
	Environment     func() []string
	OCI             OCIConfig
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
	stateDirectory  string
	skillsDirectory string
	nodeID          string
	runner          Runner
	ready           func(string, string) error
	lookPath        func(string) (string, error)
	now             func() time.Time
	random          io.Reader
	environment     func() []string
	oci             OCIConfig
	targetHelper    string
	consoles        map[string]*consoleProcess
	mutex           sync.Mutex
}

func New(config Config) (*Manager, error) {
	if config.StateDirectory == "" || config.NodeID == "" {
		return nil, fmt.Errorf("%w: state directory and node ID are required", ErrInvalid)
	}
	if config.Runner == nil {
		config.Runner = OSRunner{}
	}
	if config.ReadyMCP == nil {
		if config.Ready == nil {
			config.Ready = func(string) error { return nil }
		}
		config.ReadyMCP = func(provider, _ string) error { return config.Ready(provider) }
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
	var targetHelper string
	if config.TargetHelper != "" {
		resolved, err := canonicalExecutable(config.TargetHelper)
		if err != nil {
			return nil, fmt.Errorf("%w: target helper: %v", ErrInvalid, err)
		}
		targetHelper = resolved
	}
	parentEnvironment := config.Environment
	return &Manager{
		stateDirectory:  config.StateDirectory,
		skillsDirectory: config.SkillsDirectory,
		nodeID:          config.NodeID,
		runner:          config.Runner,
		ready:           config.ReadyMCP,
		lookPath:        config.LookPath,
		now:             config.Now,
		random:          config.Random,
		environment:     func() []string { return workerEnvironment(parentEnvironment()) },
		oci:             config.OCI,
		targetHelper:    targetHelper,
		consoles:        make(map[string]*consoleProcess),
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
	if err := manager.ready(request.Provider, request.MCPServer); err != nil {
		return Record{}, fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	if !hasNonemptyEnvironment(manager.environment(), "SNOWCAT_MCP_URL") {
		return Record{}, fmt.Errorf("%w: SNOWCAT_MCP_URL is not present in the node environment", ErrNotReady)
	}
	if !hasNonemptyEnvironment(manager.environment(), "SNOWCAT_MCP_TOKEN") {
		return Record{}, fmt.Errorf("%w: SNOWCAT_MCP_TOKEN is not present in the node environment", ErrNotReady)
	}
	accessClientID := hasNonemptyEnvironment(manager.environment(), "SNOWCAT_CF_ACCESS_CLIENT_ID")
	accessClientSecret := hasNonemptyEnvironment(manager.environment(), "SNOWCAT_CF_ACCESS_CLIENT_SECRET")
	if accessClientID != accessClientSecret {
		return Record{}, fmt.Errorf("%w: SNOWCAT_CF_ACCESS_CLIENT_ID and SNOWCAT_CF_ACCESS_CLIENT_SECRET must be present together", ErrNotReady)
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
		Provider: request.Provider, MCPServer: request.MCPServer, Model: model, Role: request.Role, Repository: request.Repository,
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
	manifest := profile.LockedManifest()
	if manager.skillsDirectory != "" {
		manifest, err = profile.ActiveManifest(manager.skillsDirectory)
		if err != nil {
			return manager.fail(record, "active worker kit inspection failed", err)
		}
	}
	canonicalKit := profile.IsCanonicalRepositorySlug(request.Repository)
	if canonicalKit {
		record.Kit = kitRecord(manifest, KitOwnershipCheckout)
		if err := manager.write(record); err != nil {
			return manager.fail(record, "canonical worker kit ownership persistence failed", err)
		}
		checkoutManifest, checkoutErr := profile.ManifestFromGit(ctx, source, baseCommit)
		if checkoutErr != nil {
			return manager.fail(record, "canonical Snowcat skill inspection failed", checkoutErr)
		}
		if baseCommit != manifest.Source.Revision {
			if _, err := manager.run(ctx, gitPath, source, nil, "merge-base", "--is-ancestor", manifest.Source.Revision, baseCommit); err != nil {
				return manager.fail(
					record,
					"canonical Snowcat checkout does not descend from the active worker kit source",
					ErrNotReady,
				)
			}
		}
		if !profile.SameSkillContent(manifest, checkoutManifest) {
			return manager.fail(record, "canonical Snowcat checkout worker kit is not the active revision", ErrNotReady)
		}
		err = profile.VerifyDirectory(checkoutManifest, filepath.Join(workspace, ".agents", "skills"))
		if err != nil {
			return manager.fail(record, "canonical Snowcat checkout does not match its recorded worker kit", err)
		}
	} else {
		skillRoots := []string{
			filepath.Join(workspace, ".agents", "skills"),
			filepath.Join(workspace, ".claude", "skills"),
		}
		if manager.skillsDirectory == "" {
			record.Kit = kitRecord(manifest, KitOwnershipCockpit)
			if err := manager.write(record); err != nil {
				return manager.fail(record, "worker kit ownership persistence failed", err)
			}
			for _, skillRoot := range skillRoots {
				if _, err := profile.InstallEmbeddedWorkspaceKit(skillRoot); err != nil {
					return manager.fail(record, "worker kit installation failed", err)
				}
			}
		} else {
			prepared, prepareErr := profile.PrepareInstallFromDirectory(manager.skillsDirectory, skillRoots)
			if prepareErr != nil {
				return manager.fail(record, "worker kit installation failed", prepareErr)
			}
			manifest = prepared.Manifest
			record.Kit = kitRecord(manifest, KitOwnershipCockpit)
			if err := manager.write(record); err != nil {
				return manager.fail(record, "worker kit ownership persistence failed", err)
			}
			_, err = prepared.Install()
			if err != nil {
				return manager.fail(record, "worker kit installation failed", err)
			}
		}
	}
	targetHelperCommand := "snowcat-cockpit"
	if manager.targetHelper != "" {
		targetHelperCommand, err = installTargetHelper(manager.targetHelper, workspace)
		if err != nil {
			return manager.fail(record, "worker target helper installation failed", err)
		}
	}
	excludePath, err := manager.writeExcludes(workerID, canonicalKit)
	if err != nil {
		return manager.fail(record, "Git exclusion setup failed", err)
	}
	if request.Adapter == AdapterOCI {
		if err := writeOCIExcludes(workspace, canonicalKit); err != nil {
			return manager.fail(record, "OCI Git exclusion setup failed", err)
		}
	}
	environment := gitEnvironment(manager.environment(), excludePath)
	if request.Adapter == AdapterOCI {
		provisioning, err := manager.provisionTools(ctx, record, runtimeSelection, ociHostEnvironment(environment))
		if err != nil {
			return manager.fail(record, "repository tool provisioning failed", err)
		}
		record.Provisioning = provisioning
	}
	prompt := buildPrompt(workerID, request.Role, request.Repository, targetHelperCommand)
	hostRelayHelper := targetHelperCommand
	if manager.targetHelper != "" {
		hostRelayHelper = filepath.Join(workspace, filepath.FromSlash(targetHelperCommand))
	}
	unlockKit := func() {}
	if manager.skillsDirectory != "" {
		unlockKit, err = profile.LockKitShared(ctx, manager.skillsDirectory)
		if err != nil {
			return manager.fail(record, "worker kit launch lock failed", err)
		}
	}
	if err := manager.verifyKitReady(request, record.Kit.Revision); err != nil {
		unlockKit()
		return manager.fail(record, "worker kit readiness changed before launch", err)
	}
	launchCommand := hostProviderCommand(request.Provider, request.MCPServer, providerPath, prompt, hostRelayHelper, workerID, workspace)
	if request.Adapter == AdapterOCI {
		launchCommand = append([]string{runtimeSelection.Path}, manager.ociArguments(record, runtimeSelection.Image, prompt)...)
		environment = ociHostEnvironment(environment)
	}
	if err := manager.startTmux(ctx, tmuxPath, record, launchCommand, environment); err != nil {
		unlockKit()
		return manager.fail(record, "tmux provider launch failed", err)
	}
	unlockKit()
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
		records[index], err = manager.syncTarget(ctx, records[index])
		if err != nil {
			return nil, err
		}
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
	record, err = manager.syncTarget(ctx, record)
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
	record, err = manager.syncTarget(ctx, record)
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

func (manager *Manager) Cleanup(ctx context.Context, workerID string, options CleanupOptions) (Record, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()

	record, err := manager.read(workerID)
	if err != nil {
		return Record{}, err
	}
	record, err = manager.syncTarget(ctx, record)
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
		excludePath, err := manager.writeExcludes(record.ID, canonicalKitRecord(record))
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
		drifted, err := removeOwnedSkills(record, options)
		if err != nil {
			return Record{}, err
		}
		if len(drifted) > 0 {
			record.Detail = "Cockpit-owned skill files drifted and were discarded: " + strings.Join(drifted, ", ") + "; "
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
	if strings.HasPrefix(record.Detail, "Cockpit-owned skill files drifted") {
		record.Detail += "workspace cleaned; branch retained"
	} else {
		record.Detail = "workspace cleaned; branch retained"
	}
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
	var stderr limitedBuffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return Console{}, fmt.Errorf("start loopback worker console: %w", err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	process := &consoleProcess{command: command, url: url}
	manager.consoles[workerID] = process
	exited := make(chan error, 1)
	go func() {
		exited <- command.Wait()
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
		select {
		case waitErr := <-exited:
			manager.stopConsoleLocked(workerID)
			detail := strings.TrimSpace(stderr.String())
			message := fmt.Sprintf("ttyd exited before the console became ready: %v", waitErr)
			if detail != "" {
				message += ": " + detail
			}
			return Console{}, fmt.Errorf("%w: %s", ErrNotReady, message)
		default:
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
	return buildPrompt(workerID, role, repository, "snowcat-cockpit")
}

func hostProviderCommand(provider, mcpServer, providerPath, prompt, helper, workerID, workspace string) []string {
	relayArguments := []string{"worker", "lease-proxy", "--worker", workerID, "--workspace", workspace}
	switch provider {
	case "codex":
		return []string{
			providerPath,
			"--config", `mcp_servers.` + mcpServer + `.enabled=false`,
			"--config", `mcp_servers.snowcat-cockpit.command=` + strconv.Quote(helper),
			"--config", `mcp_servers.snowcat-cockpit.args=` + jsonStringArray(relayArguments),
			"--config", `mcp_servers.snowcat-cockpit.env_vars=["SNOWCAT_MCP_URL","SNOWCAT_MCP_TOKEN","SNOWCAT_CF_ACCESS_CLIENT_ID","SNOWCAT_CF_ACCESS_CLIENT_SECRET"]`,
			prompt,
		}
	case "claude":
		return []string{providerPath, "--mcp-config", relayMCPConfig("stdio", helper, relayArguments), "--strict-mcp-config", prompt}
	case "copilot":
		return []string{
			providerPath, "-i", prompt,
			"--disable-mcp-server", mcpServer,
			"--additional-mcp-config", relayMCPConfig("local", helper, relayArguments),
		}
	default:
		return []string{providerPath, prompt}
	}
}

func relayMCPConfig(serverType, helper string, arguments []string) string {
	config := struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}{Servers: map[string]struct {
		Type    string   `json:"type"`
		Command string   `json:"command"`
		Args    []string `json:"args"`
	}{"snowcat-cockpit": {Type: serverType, Command: helper, Args: arguments}}}
	payload, _ := json.Marshal(config)
	return string(payload)
}

func jsonStringArray(values []string) string {
	payload, _ := json.Marshal(values)
	return string(payload)
}

func buildPrompt(workerID, role, repository, targetHelper string) string {
	leaseDiscipline := "The worker-local Cockpit relay bounds claims to 120 seconds and renews the active lease every 30 seconds only while your MCP process is alive; do not request a front-loaded lease. Continue to call heartbeat_work at the skill's normal work-step boundaries so lease loss is surfaced before more mutation. If any Snowcat tool reports SNOWCAT_COCKPIT_LEASE_LOST, stop immediately without further repository or GitHub mutation. Call the terminal lifecycle tool exactly once: the relay records separately whether complete_work was attempted and whether Snowcat acknowledged it."
	targetCommand := targetHelper + " worker target"
	pushCommand := targetHelper + " worker push-target"
	if role == "discoverer" {
		return fmt.Sprintf("Use the work-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. Work only kinds ending in -discovery for repository %s. Claim at most one item. %s Treat it as read-only discovery: do not edit files or open a GitHub artifact. Complete with concrete evidence and at most one bounded follow-up when justified. Every follow-up must declare requiredArtifact: use pull-request with write and open-pr for a change, or none for read-only work. Report the result to Snowcat, then stop.", workerID, repository, leaseDiscipline)
	}
	if role == "reviewer" {
		return fmt.Sprintf("Use the review-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. Work only pr-review items for repository %s. Claim at most one item. %s Immediately after a claim and before reading the diff or running checks, run %s --worker %s --repository %s --item <claimed item id> --kind pr-review --pull-request <review.pullRequestUrl> --head <review.headSha>. This checks out the exact bound head detached and records it. If metadata is absent or target preparation refuses a moved head, release the item as undeliverable and stop. If target preparation reports that the bound pull request is merged or closed, call block_work with that exact reason instead of releasing: nothing can be delivered and another worker would only repeat the failure. Do not switch away from the detached target. Report its structured verdict to Snowcat with complete_work, then stop.", workerID, repository, leaseDiscipline, targetCommand, workerID, repository)
	}
	return fmt.Sprintf("Use the work-snowcat-queue skill. Use worker identity %s for Snowcat lifecycle calls. For repository %s, list queued work once and claimed work once. Derive the exact claimable kind set from queued items plus claimed items whose newest attempt outcome is expired, excluding %s, and claim at most one item with only that set. %s Do not use a fixed implementation-kind whitelist; implementation, issue-resolution, fixes including pr-review-fix, pr-cure, pr-cure-change, and future worker kinds are eligible. Release-needed remains human-operated. Before substantive work on any change item, require both write and open-pr in allowedActions and requiredArtifact pull-request; if any is absent, release the item immediately as undeliverable and stop. Never infer write authority from open-pr. For pr-cure and pr-cure-change, read the root item and use its cure.pullRequestUrl and cure.headSha; for pr-review-fix use review.pullRequestUrl and review.headSha. Immediately after claiming one of those bound kinds and before inspecting or editing the tree, run %s --worker %s --repository %s --item <claimed item id> --kind <claimed kind> --pull-request <bound pull-request URL> --head <bound head SHA>. If bound metadata is absent or target preparation refuses a moved head, release the item as undeliverable and stop. If target preparation reports that the bound pull request is merged or closed, call block_work with that exact reason instead of releasing: nothing can be delivered and another worker would only repeat the failure. Run the helper only from the workspace root; a refusal is final, so do not retry it from other directories or bypass it with plain git. The helper keeps the unique local Cockpit branch at the exact bound head and records the remote target. Commit there and use %s --worker %s for every push; never use ordinary git push for bound work. For every other implementer item, keep the preallocated branch and do not create, rename, or switch branches. When write, open-pr, and requiredArtifact pull-request are all present, they authorize committing and delivery without a second permission prompt: bound work updates its existing pull request through push-target, while new-PR work pushes the current branch and opens the required draft pull request. Complete the item within its allowed actions, report the commit and pull-request artifacts to Snowcat, then stop.", workerID, repository, queueview.ImplementerExclusionPrompt(), leaseDiscipline, targetCommand, workerID, repository, pushCommand, workerID)
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
	if request.MCPServer == "" {
		request.MCPServer = "snowcat"
		if request.Provider == "copilot" {
			request.MCPServer = "snowcat-mcp"
		}
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
	if len(request.MCPServer) > 80 || !mcpServerRE.MatchString(request.MCPServer) {
		return fmt.Errorf("%w: MCP server must contain only letters, digits, _, -, or dot", ErrInvalid)
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
	if !hasNonemptyEnvironment(manager.environment(), "SNOWCAT_MCP_URL") {
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
		"SNOWCAT_MCP_TOKEN": true, "SNOWCAT_MCP_URL": true,
		"SNOWCAT_CF_ACCESS_CLIENT_ID": true, "SNOWCAT_CF_ACCESS_CLIENT_SECRET": true,
		"GH_TOKEN": true,
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
		"--cpus=4", "--pids-limit=1024", "--ulimit=core=0:0", "--log-driver=none",
		"--tmpfs=/home/cockpit:rw,size=2g,mode=1777",
		"--tmpfs=/tmp:rw,exec,size=2g,mode=1777",
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
	if record.Provisioning != nil {
		arguments = append(arguments,
			"--mount", "type=bind,source="+record.Provisioning.Cache+",destination="+MiseDataDirectory+",readonly",
		)
	}
	arguments = append(arguments,
		"--env", "SNOWCAT_MCP_TOKEN",
		"--env", "SNOWCAT_MCP_URL",
		"--env", "SNOWCAT_CF_ACCESS_CLIENT_ID",
		"--env", "SNOWCAT_CF_ACCESS_CLIENT_SECRET",
		"--env", "GH_TOKEN",
	)
	return append(arguments, image, prompt, record.Model, record.ID, record.MCPServer)
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

func canonicalExecutable(path string) (string, error) {
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
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("not an executable regular file")
	}
	return resolved, nil
}

func installTargetHelper(source, workspace string) (string, error) {
	directory := filepath.Join(workspace, ".agents", "bin")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create worker target helper directory: %w", err)
	}
	input, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open worker target helper: %w", err)
	}
	defer input.Close()
	temporary, err := os.CreateTemp(directory, ".snowcat-cockpit-*")
	if err != nil {
		return "", fmt.Errorf("create temporary worker target helper: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.Copy(temporary, input); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("copy worker target helper: %w", err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure worker target helper: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("sync worker target helper: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close worker target helper: %w", err)
	}
	destination := filepath.Join(directory, "snowcat-cockpit")
	if err := os.Rename(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("install worker target helper: %w", err)
	}
	return ".agents/bin/snowcat-cockpit", nil
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

func (manager *Manager) syncTarget(ctx context.Context, record Record) (Record, error) {
	if record.TargetedAt != nil {
		return record, nil
	}
	target, exists, err := readTarget(record.Workspace)
	if err != nil {
		return Record{}, err
	}
	if !exists {
		return record, nil
	}
	if target.WorkerID != record.ID || target.Repository != record.Repository {
		return Record{}, fmt.Errorf("%w: worker target does not match its durable record", ErrConflict)
	}
	gitPath, err := manager.lookPath("git")
	if err != nil {
		return Record{}, fmt.Errorf("sync worker target requires git: %w", err)
	}
	excludePath, err := manager.writeExcludes(record.ID, canonicalKitRecord(record))
	if err != nil {
		return Record{}, err
	}
	environment := gitEnvironment(manager.environment(), excludePath)
	output, err := manager.run(ctx, gitPath, record.Workspace, environment, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return Record{}, fmt.Errorf("inspect prepared worker target: %w", err)
	}
	currentHead := strings.ToLower(strings.TrimSpace(string(output)))
	if !headSHARE.MatchString(currentHead) {
		return Record{}, fmt.Errorf("%w: prepared worker target has an invalid Git head", ErrConflict)
	}
	if target.Mode == TargetModeDetached {
		if currentHead != target.BoundHead {
			return Record{}, fmt.Errorf("%w: detached review target moved from its bound head", ErrConflict)
		}
	} else {
		output, err := manager.run(ctx, gitPath, record.Workspace, environment, "branch", "--show-current")
		if err != nil || strings.TrimSpace(string(output)) != target.LocalBranch {
			return Record{}, fmt.Errorf("%w: prepared worker target is not on its Cockpit branch", ErrConflict)
		}
		if _, err := manager.run(ctx, gitPath, record.Workspace, environment, "merge-base", "--is-ancestor", target.BoundHead, currentHead); err != nil {
			return Record{}, fmt.Errorf("%w: prepared worker target does not descend from its bound head", ErrConflict)
		}
	}
	now := manager.now().UTC()
	record.ItemID = target.ItemID
	record.WorkKind = target.Kind
	record.PullRequestURL = target.PullRequestURL
	record.TargetRepo = target.TargetRepository
	record.TargetBranch = target.TargetBranch
	record.TargetHead = target.BoundHead
	record.TargetMode = target.Mode
	record.TargetedAt = &now
	if err := manager.write(record); err != nil {
		return Record{}, err
	}
	return record, nil
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

func (manager *Manager) writeExcludes(workerID string, canonicalKit bool) (string, error) {
	if err := os.MkdirAll(manager.recordsDirectory(), 0o700); err != nil {
		return "", fmt.Errorf("create worker state directory: %w", err)
	}
	path := manager.excludePath(workerID)
	patterns := "/.agents/\n/.claude/\n"
	if canonicalKit {
		patterns = canonicalPrivateExclusions()
	}
	if err := os.WriteFile(path, []byte(patterns), 0o600); err != nil {
		return "", fmt.Errorf("write worker Git exclusions: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", fmt.Errorf("secure worker Git exclusions: %w", err)
	}
	return path, nil
}

func canonicalPrivateExclusions() string {
	return strings.Join([]string{
		"/.agents/bin/snowcat-cockpit",
		"/.agents/bin/.snowcat-cockpit-*",
		"/.agents/cockpit-lifecycle.json",
		"/.agents/.cockpit-lifecycle-*",
		"/.agents/cockpit-target.json",
		"/.agents/.cockpit-target-*.json",
		"",
	}, "\n")
}

func canonicalKitRecord(record Record) bool {
	return record.Kit != nil && record.Kit.Ownership == KitOwnershipCheckout
}

func writeOCIExcludes(workspace string, canonicalKit bool) error {
	path := filepath.Join(workspace, ".git", "info", "exclude")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open private Git exclusions: %w", err)
	}
	defer file.Close()
	patterns := "\n/.agents/\n/.claude/\n"
	if canonicalKit {
		patterns = "\n" + canonicalPrivateExclusions()
	}
	if _, err := io.WriteString(file, patterns); err != nil {
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
	if record.MCPServer != "" && (len(record.MCPServer) > 80 || !mcpServerRE.MatchString(record.MCPServer)) {
		return errors.New("decode worker record: invalid MCP server")
	}
	if record.Role != "discoverer" && record.Role != "implementer" && record.Role != "reviewer" {
		return errors.New("decode worker record: invalid role")
	}
	if record.CreatedAt.IsZero() || record.Workspace == "" || record.Source == "" {
		return errors.New("decode worker record: incomplete lifecycle")
	}
	if record.TargetedAt == nil {
		if record.ItemID != "" || record.WorkKind != "" || record.PullRequestURL != "" || record.TargetRepo != "" || record.TargetBranch != "" || record.TargetHead != "" || record.TargetMode != "" {
			return errors.New("decode worker record: incomplete pull-request target")
		}
	} else {
		target := Target{
			Version: targetVersion, WorkerID: record.ID, Repository: record.Repository,
			ItemID: record.ItemID, Kind: record.WorkKind, PullRequestURL: record.PullRequestURL,
			BoundHead: record.TargetHead, LeaseHead: record.TargetHead,
			TargetRepository: record.TargetRepo, TargetBranch: record.TargetBranch,
			Mode: record.TargetMode,
		}
		if record.TargetMode == TargetModeBranch {
			target.LocalBranch = record.Branch
		}
		if err := validateTarget(target); err != nil {
			return errors.New("decode worker record: invalid pull-request target")
		}
	}
	return nil
}

func kitRecord(manifest profile.Manifest, ownership string) *KitRecord {
	skills := make(map[string]string, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		skills[skill.Name] = skill.SHA256
	}
	return &KitRecord{Revision: manifest.Source.Revision, Skills: skills, Ownership: ownership}
}

// removeOwnedSkills deletes the Cockpit-owned skill files a worker was given.
// A file is Cockpit's when its digest matches the worker's recorded kit (or,
// for a record that predates the kit field, the node's current lock). Any
// other content is drift: refused unless the caller discards it explicitly,
// in which case the skill names are returned for the record.
func removeOwnedSkills(record Record, options CleanupOptions) ([]string, error) {
	if canonicalKitRecord(record) {
		return nil, nil
	}
	manifest := profile.LockedManifest()
	expected := make(map[string]string, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		expected[skill.Name] = skill.SHA256
	}
	if record.Kit != nil {
		for name, digest := range record.Kit.Skills {
			expected[name] = digest
		}
	}
	names := make([]string, 0, len(expected))
	for name := range expected {
		names = append(names, name)
	}
	sort.Strings(names)
	drifted := []string{}
	for _, providerRoot := range []string{".agents", ".claude"} {
		for _, name := range names {
			path := filepath.Join(record.Workspace, providerRoot, "skills", name, "SKILL.md")
			content, err := os.ReadFile(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("inspect Cockpit-owned skill before cleanup: %w", err)
			}
			digest := sha256.Sum256(content)
			if hex.EncodeToString(digest[:]) != expected[name] {
				if !options.DiscardDriftedSkills {
					return nil, fmt.Errorf("%w: Cockpit-owned skill path drifted (%s/skills/%s); rerun with --discard-drifted-skills to discard it", ErrConflict, providerRoot, name)
				}
				drifted = append(drifted, providerRoot+"/skills/"+name)
			}
			if err := os.Remove(path); err != nil {
				return nil, fmt.Errorf("remove Cockpit-owned skill: %w", err)
			}
			_ = os.Remove(filepath.Dir(path))
		}
		_ = os.Remove(filepath.Join(record.Workspace, providerRoot, "skills"))
		_ = os.Remove(filepath.Join(record.Workspace, providerRoot))
	}
	return drifted, nil
}

func (manager *Manager) verifyKitReady(request LaunchRequest, revision string) error {
	if manager.skillsDirectory != "" {
		before, err := profile.ActiveManifest(manager.skillsDirectory)
		if err != nil {
			return fmt.Errorf("%w: inspect active worker kit: %v", ErrNotReady, err)
		}
		if before.Source.Revision != revision {
			return fmt.Errorf("%w: prepared worker kit %s is not the active revision %s", ErrNotReady, revision, before.Source.Revision)
		}
	}
	if err := manager.ready(request.Provider, request.MCPServer); err != nil {
		return fmt.Errorf("%w: %v", ErrNotReady, err)
	}
	if manager.skillsDirectory != "" {
		after, err := profile.ActiveManifest(manager.skillsDirectory)
		if err != nil {
			return fmt.Errorf("%w: inspect active worker kit after readiness: %v", ErrNotReady, err)
		}
		if after.Source.Revision != revision {
			return fmt.Errorf("%w: active worker kit changed from prepared revision %s to %s", ErrNotReady, revision, after.Source.Revision)
		}
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
