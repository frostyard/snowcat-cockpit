package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	source         string
	dead           bool
	dirty          bool
	session        bool
	commands       []Command
	workspace      string
	baseCommit     string
	remoteURL      string
	upstream       string
	ahead          int
	behind         int
	dockerSecurity string
	currentBranch  string
	currentHead    string
	pinFiles       map[string]string
	provisionFails bool
	provisionRuns  int
}

type loggingRunner struct {
	t *testing.T
}

func (runner loggingRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	output, err := (OSRunner{}).Run(ctx, command)
	if err != nil {
		runner.t.Logf("command failed: %s %#v: %v: %s", command.Name, command.Arguments, err, output)
	}
	return output, err
}

func TestOSManagerCreatesRetainsAndCleansRealWorktreeAndTmux(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}

	root := t.TempDir()
	probePath := filepath.Join(root, "socket-probe")
	probe, err := net.Listen("unix", probePath)
	if err != nil {
		t.Skipf("Unix sockets are unavailable in this test sandbox: %v", err)
	}
	if err := probe.Close(); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(probePath)
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runGit := func(arguments ...string) {
		command := exec.Command(gitPath, append([]string{"-C", source}, arguments...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("managed worker trial\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "README.md")
	runGit("-c", "user.name=Cockpit Test", "-c", "user.email=cockpit@example.invalid", "commit", "-q", "-m", "initial")

	providerPath := filepath.Join(root, "codex")
	if err := os.WriteFile(providerPath, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"),
		NodeID:         "node-real-lifecycle-test",
		Runner:         loggingRunner{t: t},
		Environment: func() []string {
			return []string{"PATH=" + os.Getenv("PATH"), "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret"}
		},
		LookPath: func(name string) (string, error) {
			switch name {
			case "git":
				return gitPath, nil
			case "tmux":
				return tmuxPath, nil
			case "codex":
				return providerPath, nil
			default:
				return "", exec.ErrNotFound
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		command := exec.Command(tmuxPath, "-S", manager.socketPath(record.ID), "kill-server")
		_ = command.Run()
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		records, err := manager.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(records) == 1 && records[0].Status == StatusExited {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider did not exit into retained state: %#v", records)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := manager.AttachCommand(context.Background(), record.ID); err != nil {
		t.Fatalf("retained terminal is not attachable: %v", err)
	}
	cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Status != StatusCleaned {
		t.Fatalf("cleaned = %#v", cleaned)
	}
	if _, err := os.Stat(record.Workspace); !os.IsNotExist(err) {
		t.Fatalf("real worktree remains after cleanup: %v", err)
	}
}

func (runner *fakeRunner) Run(_ context.Context, command Command) ([]byte, error) {
	runner.commands = append(runner.commands, command)
	arguments := strings.Join(command.Arguments, "\x00")
	switch {
	case strings.Contains(arguments, "rev-parse\x00--show-toplevel"):
		return []byte(runner.source + "\n"), nil
	case arguments == "rev-parse\x00HEAD^{commit}":
		return []byte(runner.currentHead + "\n"), nil
	case strings.Contains(arguments, "rev-parse\x00--verify"):
		return []byte(runner.baseCommit + "\n"), nil
	case strings.Contains(arguments, "rev-parse\x00--abbrev-ref\x00--symbolic-full-name"):
		if runner.upstream == "" {
			return nil, errors.New("no upstream")
		}
		return []byte(runner.upstream + "\n"), nil
	case strings.Contains(arguments, "rev-list\x00--left-right\x00--count"):
		return []byte(fmt.Sprintf("%d\t%d\n", runner.ahead, runner.behind)), nil
	case strings.Contains(arguments, "remote\x00get-url\x00--push\x00origin"):
		if runner.remoteURL == "" {
			return nil, errors.New("missing origin")
		}
		return []byte(runner.remoteURL + "\n"), nil
	case strings.Contains(arguments, "clone\x00--local\x00--no-hardlinks\x00--no-checkout"):
		runner.workspace = command.Arguments[len(command.Arguments)-1]
		if err := os.MkdirAll(filepath.Join(runner.workspace, ".git", "info"), 0o700); err != nil {
			return nil, err
		}
		for name, content := range runner.pinFiles {
			if err := os.WriteFile(filepath.Join(runner.workspace, name), []byte(content), 0o600); err != nil {
				return nil, err
			}
		}
		if err := os.WriteFile(filepath.Join(runner.workspace, ".git", "info", "exclude"), nil, 0o600); err != nil {
			return nil, err
		}
		return nil, nil
	case strings.Contains(arguments, "remote\x00set-url\x00origin"):
		return nil, nil
	case strings.Contains(arguments, "checkout\x00-b"):
		return nil, nil
	case strings.Contains(arguments, "fetch\x00--no-tags"):
		return nil, nil
	case strings.Contains(arguments, "worktree\x00add"):
		runner.workspace = command.Arguments[len(command.Arguments)-2]
		if err := os.MkdirAll(runner.workspace, 0o700); err != nil {
			return nil, err
		}
		return nil, nil
	case strings.Contains(arguments, "info\x00--format\x00{{.Host.Security.Rootless}}"):
		return []byte("true\n"), nil
	case strings.Contains(arguments, "info\x00--format\x00{{.OSType}}\n{{json .SecurityOptions}}"):
		security := runner.dockerSecurity
		if security == "" {
			security = `["name=seccomp,profile=builtin"]`
		}
		return []byte("linux\n" + security + "\n"), nil
	case strings.Contains(arguments, "image\x00exists"):
		return nil, nil
	case strings.Contains(arguments, "--entrypoint\x00/bin/sh") && strings.Contains(arguments, "MISE_LOCKED=1"):
		runner.provisionRuns++
		if runner.provisionFails {
			return []byte("mise ERROR Failed to install aqua:jqlang/jq@1.7.1: jq@1.7.1 is not in the lockfile\nmise ERROR Version: 2026.8.12\nmise ERROR Run with --verbose\n"), errors.New("exit status 1")
		}
		return []byte("mise go@1.26.7 ✓ installed\n{\"go\":[{\"version\":\"1.26.7\"}],\"golangci-lint\":[{\"version\":\"2.13.1\"}]}\n"), nil
	case strings.Contains(arguments, "image\x00inspect"):
		return []byte("[]\n"), nil
	case strings.Contains(arguments, "stop\x00--ignore"):
		return nil, nil
	case strings.Contains(arguments, "stop\x00--time"):
		return nil, nil
	case strings.Contains(arguments, "new-session"):
		runner.session = true
		return nil, nil
	case strings.Contains(arguments, "display-message"):
		if !runner.session {
			return nil, errors.New("missing session")
		}
		if runner.dead {
			return []byte("1\n"), nil
		}
		return []byte("0\n"), nil
	case strings.Contains(arguments, "has-session"):
		if runner.session {
			return nil, nil
		}
		return nil, errors.New("missing session")
	case strings.Contains(arguments, "kill-server"):
		runner.session = false
		return nil, nil
	case strings.Contains(arguments, "status\x00--porcelain"):
		if runner.dirty {
			return []byte(" M changed.go\n"), nil
		}
		return nil, nil
	case arguments == "branch\x00--show-current":
		return []byte(runner.currentBranch + "\n"), nil
	case strings.Contains(arguments, "merge-base\x00--is-ancestor"):
		return nil, nil
	case strings.Contains(arguments, "worktree\x00remove"):
		return nil, os.RemoveAll(command.Arguments[len(command.Arguments)-1])
	default:
		return nil, errors.New("unexpected command")
	}
}

func TestManagedWorkerLifecyclePreservesSecretsAndWorkspaceUntilCleanup(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	targetHelper := filepath.Join(root, "snowcat-cockpit")
	if err := os.WriteFile(targetHelper, []byte("target-helper-fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("a", 40)}
	now := time.Date(2026, 8, 21, 17, 30, 0, 0, time.UTC)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"),
		NodeID:         "node-0123456789abcdef0123456789abcdef",
		TargetHelper:   targetHelper,
		Runner:         runner,
		Ready:          func(string) error { return nil },
		LookPath:       func(name string) (string, error) { return "/tools/" + name, nil },
		Now:            func() time.Time { return now },
		Random:         bytes.NewReader([]byte("12345678")),
		Environment: func() []string {
			return []string{
				"PATH=/tools",
				"SECRET_SENTINEL=never-persist",
				"SNOWCAT_MCP_URL=https://snowcat.example/mcp",
				"SNOWCAT_MCP_TOKEN=worker-secret",
				"SNOWCAT_COCKPIT_MCP_URL=https://snowcat.example/mcp",
				"SNOWCAT_COCKPIT_MCP_TOKEN=observer-secret",
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	record, err := manager.Launch(context.Background(), LaunchRequest{
		Provider: "claude", Role: "implementer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusRunning || record.Branch != "cockpit/"+record.ID {
		t.Fatalf("record = %#v", record)
	}
	if _, err := os.Stat(filepath.Join(record.Workspace, ".agents", "skills", "work-snowcat-queue", "SKILL.md")); err != nil {
		t.Fatalf("locked worker kit not installed: %v", err)
	}
	installedHelper := filepath.Join(record.Workspace, ".agents", "bin", "snowcat-cockpit")
	if content, err := os.ReadFile(installedHelper); err != nil || string(content) != "target-helper-fixture" {
		t.Fatalf("worker target helper = %q, %v", content, err)
	}
	if info, err := os.Stat(installedHelper); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("worker target helper mode = %v, %v", info, err)
	}
	stored, err := os.ReadFile(manager.recordPath(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("never-persist")) {
		t.Fatal("worker record contains an inherited environment value")
	}
	launchCommand := runner.commands[len(runner.commands)-1]
	if strings.Contains(strings.Join(launchCommand.Arguments, "\n"), "never-persist") {
		t.Fatal("tmux argv contains an inherited environment value")
	}
	joinedEnvironment := strings.Join(launchCommand.Env, "\n")
	if !strings.Contains(joinedEnvironment, "SECRET_SENTINEL=never-persist") {
		t.Fatal("ordinary inherited worker environment was removed")
	}
	if strings.Contains(joinedEnvironment, "SNOWCAT_COCKPIT_MCP_") || strings.Contains(joinedEnvironment, "observer-secret") {
		t.Fatal("Cockpit observer configuration entered the worker environment")
	}
	joined := strings.Join(launchCommand.Arguments, "\n")
	if !strings.Contains(joined, "work-snowcat-queue") || !strings.Contains(joined, record.ID) || !strings.Contains(joined, "-fix") || !strings.Contains(joined, ".agents/bin/snowcat-cockpit worker target") {
		t.Fatalf("bounded prompt missing from tmux launch: %s", joined)
	}

	runner.dead = true
	records, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Status != StatusExited {
		t.Fatalf("reconciled records = %#v", records)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("exited workspace was not retained: %v", err)
	}

	runner.dirty = true
	if _, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dirty cleanup error = %v", err)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("dirty workspace was removed: %v", err)
	}
	runner.dirty = false
	cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Status != StatusCleaned {
		t.Fatalf("cleaned record = %#v", cleaned)
	}
	if _, err := os.Stat(record.Workspace); !os.IsNotExist(err) {
		t.Fatalf("workspace remains after explicit cleanup: %v", err)
	}
	if _, err := os.Stat(manager.recordPath(record.ID)); err != nil {
		t.Fatalf("cleaned lifecycle record was not retained: %v", err)
	}
}

func TestManagerPersistsPreparedPullRequestTarget(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	boundHead := strings.Repeat("a", 40)
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("b", 40)}
	now := time.Date(2026, 8, 23, 16, 0, 0, 0, time.UTC)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-target-test", Runner: runner,
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Now:      nowUTC(now), Random: bytes.NewReader([]byte("target!!")),
		Environment: func() []string {
			return []string{"PATH=/tools", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Provider: "codex", Role: "implementer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	runner.currentBranch = record.Branch
	runner.currentHead = boundHead
	if err := writeTarget(record.Workspace, Target{
		Version: targetVersion, WorkerID: record.ID, Repository: record.Repository,
		ItemID: "01234567-89ab-cdef-0123-456789abcdef", Kind: "pr-cure-change",
		PullRequestURL: "https://github.com/frostyard/firn/pull/42",
		BoundHead:      boundHead, LeaseHead: boundHead, TargetRepository: "frostyard/firn",
		TargetBranch: "feature/cure", LocalBranch: record.Branch, Mode: TargetModeBranch,
	}); err != nil {
		t.Fatal(err)
	}
	got, err := manager.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ItemID == "" || got.WorkKind != "pr-cure-change" || got.TargetBranch != "feature/cure" || got.TargetHead != boundHead || got.TargetedAt == nil {
		t.Fatalf("targeted record = %#v", got)
	}
	stored, err := manager.read(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.TargetedAt == nil || stored.PullRequestURL != "https://github.com/frostyard/firn/pull/42" {
		t.Fatalf("stored record = %#v", stored)
	}
}

func nowUTC(value time.Time) func() time.Time {
	return func() time.Time { return value }
}

func TestOCIWorkerLaunchUsesOnlyTheBoundedRootlessPodmanProjection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex")
	ghConfig := filepath.Join(root, "gh")
	for _, path := range []string{
		filepath.Join(codexHome, "auth.json"), filepath.Join(codexHome, "config.toml"),
		filepath.Join(ghConfig, "hosts.yml"), filepath.Join(ghConfig, "config.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture must never be read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("c", 40), remoteURL: "git@github.com:frostyard/firn.git"}
	image := "sha256:" + strings.Repeat("d", 64)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-oci-test",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random:   bytes.NewReader([]byte("87654321")),
		Environment: func() []string {
			return []string{"PATH=/tools", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000", "SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "GH_TOKEN=github-never-in-argv", "SECRET_SENTINEL=must-be-filtered"}
		},
		OCI: OCIConfig{Images: map[string]string{"codex": image}, CodexHome: codexHome, GHConfigDir: ghConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Adapter != AdapterOCI || record.Model != OCIModelReview || record.Status != StatusRunning {
		t.Fatalf("record = %#v", record)
	}
	excludes, err := os.ReadFile(filepath.Join(record.Workspace, ".git", "info", "exclude"))
	if err != nil || !bytes.Contains(excludes, []byte("/.agents/")) || !bytes.Contains(excludes, []byte("/.claude/")) {
		t.Fatalf("OCI-private exclusions = %q, %v", excludes, err)
	}
	for _, command := range runner.commands {
		if slices.Contains(command.Arguments, "worktree") {
			t.Fatalf("OCI allocation used a linked worktree: %#v", command.Arguments)
		}
		if slices.Contains(command.Arguments, "set-url") && !slices.Contains(command.Arguments, "https://github.com/frostyard/firn.git") {
			t.Fatalf("OCI origin was not projected to canonical HTTPS: %#v", command.Arguments)
		}
	}
	launch := runner.commands[len(runner.commands)-1]
	argv := strings.Join(launch.Arguments, "\n")
	for _, required := range []string{
		"/tools/podman", "--pull=never", "--read-only", "--read-only-tmpfs=false",
		"--userns=keep-id:uid=1000,gid=1000", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--cpus=4", "--pids-limit=1024", "--ulimit=core=0:0", "--log-driver=none", "--env",
		"--tmpfs=/tmp:rw,exec,size=2g,mode=1777",
		"SNOWCAT_MCP_TOKEN", "SNOWCAT_MCP_URL", "GH_TOKEN", image, OCIModelReview, record.ID, record.Workspace, codexHome, ghConfig,
	} {
		if !strings.Contains(argv, required) {
			t.Errorf("Podman launch is missing %q: %s", required, argv)
		}
	}
	for _, forbidden := range []string{"never-in-argv", "github-never-in-argv", "SECRET_SENTINEL", "--privileged", "--network=host", "/run/user/1000/podman/podman.sock"} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("Podman launch contains forbidden %q: %s", forbidden, argv)
		}
	}
	joinedEnvironment := strings.Join(launch.Env, "\n")
	if !strings.Contains(joinedEnvironment, "SNOWCAT_MCP_TOKEN=never-in-argv") || !strings.Contains(joinedEnvironment, "GH_TOKEN=github-never-in-argv") || strings.Contains(joinedEnvironment, "SECRET_SENTINEL") {
		t.Fatalf("bounded OCI host environment = %q", joinedEnvironment)
	}
	stored, err := os.ReadFile(manager.recordPath(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("never-in-argv")) || bytes.Contains(stored, []byte("github-never-in-argv")) || bytes.Contains(stored, []byte("fixture must never be read")) {
		t.Fatal("OCI worker record contains credential material")
	}
	for _, command := range runner.commands {
		if strings.HasSuffix(command.Name, "/podman") && strings.Contains(strings.Join(command.Env, "\n"), "SECRET_SENTINEL") {
			t.Fatalf("Podman command inherited an unrelated environment value: %#v", command.Env)
		}
	}
	if _, err := manager.Stop(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	var stop Command
	for _, command := range runner.commands {
		if strings.HasSuffix(command.Name, "/podman") && slices.Contains(command.Arguments, "stop") {
			stop = command
		}
	}
	if !slices.Contains(stop.Arguments, manager.containerName(record.ID)) {
		t.Fatalf("OCI stop did not address the exact container: %#v", stop.Arguments)
	}
	cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cleaned.Status != StatusCleaned {
		t.Fatalf("cleaned OCI record = %#v", cleaned)
	}
	if _, err := os.Stat(record.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleaned OCI workspace remains: %v", err)
	}
	refspec := "refs/heads/" + record.Branch + ":refs/heads/" + record.Branch
	foundFetch := false
	for _, command := range runner.commands {
		foundFetch = foundFetch || slices.Contains(command.Arguments, "fetch") && slices.Contains(command.Arguments, refspec)
	}
	if !foundFetch {
		t.Fatalf("OCI cleanup did not retain the exact branch: %#v", runner.commands)
	}
}

func TestCopilotOCIWorkerUsesOnlyItsProviderProjection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	copilotHome := filepath.Join(root, "copilot")
	ghConfig := filepath.Join(root, "gh")
	for _, path := range []string{
		filepath.Join(copilotHome, "mcp-config.json"),
		filepath.Join(ghConfig, "hosts.yml"), filepath.Join(ghConfig, "config.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture must never be read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("f", 40), remoteURL: "git@github.com:frostyard/firn.git"}
	image := "sha256:" + strings.Repeat("a", 64)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-copilot-oci-test",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random:   bytes.NewReader([]byte("copilot!")),
		Environment: func() []string {
			return []string{"PATH=/tools", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000", "SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "GH_TOKEN=github-never-in-argv"}
		},
		OCI: OCIConfig{
			Images: map[string]string{"copilot": image}, CopilotHome: copilotHome, GHConfigDir: ghConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Provider: "copilot", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Provider != "copilot" || record.Model != OCIModelAuto || record.Status != StatusRunning {
		t.Fatalf("record = %#v", record)
	}
	launch := runner.commands[len(runner.commands)-1]
	argv := strings.Join(launch.Arguments, "\n")
	for _, required := range []string{
		image, OCIModelAuto, copilotHome,
		"/run/cockpit/input/copilot/mcp-config.json", "SNOWCAT_MCP_TOKEN", "SNOWCAT_MCP_URL", "GH_TOKEN", record.ID,
		"--tmpfs=/home/cockpit/.cache/copilot:rw,exec,size=512m,mode=1777",
	} {
		if !strings.Contains(argv, required) {
			t.Errorf("Copilot Podman launch is missing %q: %s", required, argv)
		}
	}
	for _, forbidden := range []string{"/run/cockpit/input/codex", "never-in-argv", "github-never-in-argv"} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("Copilot Podman launch contains forbidden %q: %s", forbidden, argv)
		}
	}
	stored, err := os.ReadFile(manager.recordPath(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("never-in-argv")) || bytes.Contains(stored, []byte("fixture must never be read")) {
		t.Fatal("Copilot OCI worker record contains credential material")
	}
}

func TestDockerOCIWorkerUsesExplicitRootfulDaemonBoundary(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex")
	ghConfig := filepath.Join(root, "gh")
	for _, path := range []string{
		filepath.Join(codexHome, "auth.json"), filepath.Join(codexHome, "config.toml"),
		filepath.Join(ghConfig, "hosts.yml"), filepath.Join(ghConfig, "config.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture must never be read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("e", 40), remoteURL: "git@github.com:frostyard/firn.git"}
	image := "sha256:" + strings.Repeat("9", 64)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-docker-oci-test",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random:   bytes.NewReader([]byte("docker!!")),
		Environment: func() []string {
			return []string{"PATH=/tools", "HOME=/home/test", "SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "GH_TOKEN=github-never-in-argv", "SECRET_SENTINEL=filtered"}
		},
		OCI: OCIConfig{
			DockerImages: map[string]string{"codex": image}, DockerAddHost: "snowcat.goat-snake.ts.net:100.108.168.44", CodexHome: codexHome, GHConfigDir: ghConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Runtime: RuntimeDocker, Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Runtime != RuntimeDocker || record.RuntimePosture != PostureRootful || record.Model != OCIModelReview || record.Status != StatusRunning {
		t.Fatalf("record = %#v", record)
	}
	launch := runner.commands[len(runner.commands)-1]
	argv := strings.Join(launch.Arguments, "\n")
	for _, required := range []string{
		"/tools/docker", "--pull=never", "--read-only", "--user=1000:1000",
		"--cap-drop=ALL", "--security-opt=no-new-privileges=true", "--cpus=4", "--pids-limit=1024", "--ulimit=core=0:0", "--log-driver=none",
		"--tmpfs=/tmp:rw,exec,size=2g,mode=1777",
		"--add-host", "snowcat.goat-snake.ts.net:100.108.168.44", "readonly", "SNOWCAT_MCP_TOKEN", "SNOWCAT_MCP_URL", "GH_TOKEN", image, record.ID, record.Workspace,
	} {
		if !strings.Contains(argv, required) {
			t.Errorf("Docker launch is missing %q: %s", required, argv)
		}
	}
	for _, forbidden := range []string{
		"--read-only-tmpfs=false", "--userns=keep-id", "rw=true",
		"/home/cockpit/.cache/copilot", "never-in-argv", "github-never-in-argv", "SECRET_SENTINEL", "--privileged", "--network=host",
	} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("Docker launch contains forbidden %q: %s", forbidden, argv)
		}
	}
	if _, err := manager.Stop(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	foundStop := false
	for _, command := range runner.commands {
		if strings.HasSuffix(command.Name, "/docker") && slices.Contains(command.Arguments, "stop") {
			foundStop = slices.Contains(command.Arguments, manager.containerName(record.ID))
		}
	}
	if !foundStop {
		t.Fatalf("Docker stop did not address the exact container: %#v", runner.commands)
	}
}

func TestClaudeOCIWorkerUsesOnlyItsProviderProjection(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeHome := filepath.Join(root, "claude")
	ghConfig := filepath.Join(root, "gh")
	for _, path := range []string{
		filepath.Join(claudeHome, ".credentials.json"),
		filepath.Join(ghConfig, "hosts.yml"), filepath.Join(ghConfig, "config.yml"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture must never be read"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("1", 40), remoteURL: "git@github.com:frostyard/firn.git"}
	image := "sha256:" + strings.Repeat("b", 64)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-claude-oci-test",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random:   bytes.NewReader([]byte("claude!!")),
		Environment: func() []string {
			return []string{
				"PATH=/tools", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000",
				"SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp",
				"GH_TOKEN=github-never-in-argv", "SECRET_SENTINEL=filtered",
			}
		},
		OCI: OCIConfig{
			Images: map[string]string{"claude": image}, ClaudeHome: claudeHome, GHConfigDir: ghConfig,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Provider: "claude", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Provider != "claude" || record.Model != OCIModelOpus || record.Status != StatusRunning {
		t.Fatalf("record = %#v", record)
	}
	launch := runner.commands[len(runner.commands)-1]
	argv := strings.Join(launch.Arguments, "\n")
	for _, required := range []string{
		image, OCIModelOpus, claudeHome, "/run/cockpit/input/claude/.credentials.json",
		"SNOWCAT_MCP_TOKEN", "SNOWCAT_MCP_URL", "GH_TOKEN",
		"--tmpfs=/home/cockpit:rw,size=2g,mode=1777",
	} {
		if !strings.Contains(argv, required) {
			t.Errorf("Claude Podman launch is missing %q: %s", required, argv)
		}
	}
	for _, forbidden := range []string{
		"/run/cockpit/input/codex", "/run/cockpit/input/copilot",
		"/home/cockpit/.cache/copilot", "never-in-argv", "https://snowcat.invalid/mcp", "github-never-in-argv", "SECRET_SENTINEL",
	} {
		if strings.Contains(argv, forbidden) {
			t.Errorf("Claude Podman launch contains forbidden %q: %s", forbidden, argv)
		}
	}
	joinedEnvironment := strings.Join(launch.Env, "\n")
	for _, required := range []string{
		"SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "GH_TOKEN=github-never-in-argv",
	} {
		if !strings.Contains(joinedEnvironment, required) {
			t.Errorf("Claude Podman environment is missing %q: %s", required, joinedEnvironment)
		}
	}
	if strings.Contains(joinedEnvironment, "SECRET_SENTINEL") {
		t.Fatalf("Claude Podman environment contains unrelated data: %s", joinedEnvironment)
	}
	stored, err := os.ReadFile(manager.recordPath(record.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte("never-in-argv")) || bytes.Contains(stored, []byte("fixture must never be read")) {
		t.Fatal("Claude OCI worker record contains credential material")
	}
}

func TestOCIPrivateInputsRejectLoosePermissionsAndSymlinks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	loose := filepath.Join(root, "loose")
	if err := os.WriteFile(loose, []byte("fixture"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateInput(loose); err == nil {
		t.Fatal("loosely permissioned OCI input was accepted")
	}
	private := filepath.Join(root, "private")
	if err := os.WriteFile(private, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if err := validatePrivateInput(link); err == nil {
		t.Fatal("symlinked OCI input was accepted")
	}
}

func TestInspectBaseReportsLocalUpstreamRelationWithoutFetching(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{
		source: source, baseCommit: strings.Repeat("2", 40), upstream: "origin/main", ahead: 1, behind: 3,
	}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-base-test", Runner: runner,
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.InspectBase(context.Background(), source, "main")
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Status != "diverged" || inspection.Ahead != 1 || inspection.Behind != 3 || inspection.Upstream != "origin/main" {
		t.Fatalf("inspection = %#v", inspection)
	}
	for _, command := range runner.commands {
		if slices.Contains(command.Arguments, "fetch") || slices.Contains(command.Arguments, "pull") {
			t.Fatalf("base inspection mutated remote refs: %#v", command.Arguments)
		}
	}
}

func TestOCIRemoteProjectsTheSelectedGitHubRepositoryToHTTPS(t *testing.T) {
	t.Parallel()

	for _, remote := range []string{
		"https://token@github.com/frostyard/firn.git",
		"https://user:secret@github.com/frostyard/firn.git",
		"https://github.com/frostyard/other.git",
		"git@gitlab.com:frostyard/firn.git",
		"/home/test/source",
		"file:///home/test/source",
	} {
		if projected, err := projectGitHubRemote(remote, "frostyard/firn"); err == nil {
			t.Errorf("projectGitHubRemote(%q) = %q", remote, projected)
		}
	}
	for _, remote := range []string{
		"https://github.com/frostyard/firn.git",
		"ssh://git@github.com/frostyard/firn.git",
		"git@github.com:frostyard/firn.git",
	} {
		projected, err := projectGitHubRemote(remote, "frostyard/firn")
		if err != nil || projected != "https://github.com/frostyard/firn.git" {
			t.Errorf("projectGitHubRemote(%q) = %q, %v", remote, projected, err)
		}
	}
}

func TestOCIReadinessFailsBeforeAllocatingAWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("e", 40)}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-oci-not-ready",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Environment: func() []string {
			return []string{"PATH=/tools", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret"}
		},
		OCI: OCIConfig{Images: map[string]string{"codex": "not-pinned"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("OCI launch error = %v", err)
	}
	if runner.workspace != "" {
		t.Fatalf("OCI readiness failure allocated workspace %q", runner.workspace)
	}
}

func TestInvalidDockerHostMappingFailsBeforeAllocatingAWorkspace(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("e", 40)}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-docker-host-not-ready",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Environment: func() []string {
			return []string{"PATH=/tools", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret"}
		},
		OCI: OCIConfig{
			DockerImages:  map[string]string{"codex": "sha256:" + strings.Repeat("8", 64)},
			DockerAddHost: "snowcat.goat-snake.ts.net:not-an-ip",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Launch(context.Background(), LaunchRequest{
		Adapter: AdapterOCI, Runtime: RuntimeDocker, Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source,
	})
	if !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "Docker host mapping") {
		t.Fatalf("Docker host-mapping launch error = %v", err)
	}
	if runner.workspace != "" {
		t.Fatalf("Docker host-mapping readiness failure allocated workspace %q", runner.workspace)
	}
}

func TestStopRetainsWorkspaceAndAttachUsesExactSocket(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("b", 40)}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-test",
		Runner: runner, LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random: bytes.NewReader([]byte("abcdefgh")), Environment: func() []string {
			return []string{"PATH=/tools", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret"}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	attach, err := manager.AttachCommand(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(attach.Arguments, manager.socketPath(record.ID)) {
		t.Fatalf("attach args = %#v", attach.Arguments)
	}
	stopped, err := manager.Stop(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != StatusStopped {
		t.Fatalf("stopped = %#v", stopped)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("stop removed workspace: %v", err)
	}
}

func TestReadLegacyOCIRecordDefaultsToRootlessPodman(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manager, err := New(Config{StateDirectory: filepath.Join(root, "state"), NodeID: "node-legacy-record-test"})
	if err != nil {
		t.Fatal(err)
	}
	workerID := "worker-0123456789abcdef"
	if err := os.MkdirAll(manager.recordsDirectory(), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := `{
  "version": 1,
  "id": "worker-0123456789abcdef",
  "nodeId": "node-legacy-record-test",
  "adapter": "oci",
  "provider": "codex",
  "role": "reviewer",
  "repository": "frostyard/firn",
  "source": "/tmp/source",
  "workspace": "/tmp/workspace",
  "baseRef": "main",
  "baseCommit": "0123456789012345678901234567890123456789",
  "branch": "cockpit/worker-0123456789abcdef",
  "status": "stopped",
  "detail": "legacy fixture",
  "createdAt": "2026-08-21T12:00:00Z"
}`
	if err := os.WriteFile(manager.recordPath(workerID), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	record, err := manager.Get(context.Background(), workerID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Runtime != RuntimePodman || record.RuntimePosture != PostureRootless {
		t.Fatalf("legacy OCI record = %#v", record)
	}
}

func TestBuildPromptPinsRoleSelections(t *testing.T) {
	t.Parallel()

	discoverer := BuildPrompt("worker-1234567890abcdef", "discoverer", "frostyard/firn")
	for _, expected := range []string{"work-snowcat-queue", "only kinds ending in -discovery", "read-only discovery", "requiredArtifact", "open-pr", "bounds claims to 120 seconds", "renews the active lease every 30 seconds", "at most one"} {
		if !strings.Contains(discoverer, expected) {
			t.Fatalf("discoverer prompt missing %q: %s", expected, discoverer)
		}
	}
	implementer := BuildPrompt("worker-1234567890abcdef", "implementer", "frostyard/firn")
	for _, expected := range []string{"work-snowcat-queue", "list queued work once and claimed work once", "newest attempt outcome is expired", "excluding kinds ending in -discovery and exact pr-review and release-needed", "Do not use a fixed implementation-kind whitelist", "issue-resolution", "pr-review-fix, pr-cure, pr-cure-change", "worker target", "cure.pullRequestUrl", "review.pullRequestUrl", "refuses a moved head", "merged or closed, call block_work", "do not retry it from other directories", "push-target", "never use ordinary git push", "do not create, rename, or switch branches", "write and open-pr in allowedActions", "requiredArtifact pull-request", "Never infer write authority from open-pr", "without a second permission prompt", "do not request a front-loaded lease", "SNOWCAT_COCKPIT_LEASE_LOST", "whether complete_work was attempted", "at most one"} {
		if !strings.Contains(implementer, expected) {
			t.Fatalf("implementer prompt missing %q: %s", expected, implementer)
		}
	}
	reviewer := BuildPrompt("worker-1234567890abcdef", "reviewer", "frostyard/firn")
	for _, expected := range []string{"review-snowcat-queue", "only pr-review", "bounds claims to 120 seconds", "worker target", "exact bound head detached", "release the item", "merged or closed, call block_work", "result.artifacts and followUps as empty arrays ([])", "tool schema requires both"} {
		if !strings.Contains(reviewer, expected) {
			t.Fatalf("reviewer prompt missing %q: %s", expected, reviewer)
		}
	}
}

func TestHostProviderCommandsReplaceDirectSnowcatWithWorkerLocalRelay(t *testing.T) {
	t.Parallel()

	helper := "/workspace/.agents/bin/snowcat-cockpit"
	workspace := "/workspace"
	for _, provider := range []string{"codex", "claude", "copilot"} {
		directServer := "snowcat"
		if provider == "copilot" {
			directServer = "snowcat-mcp"
		}
		command := hostProviderCommand(provider, directServer, "/tools/"+provider, "bounded prompt", helper, "worker-1234567890abcdef", workspace)
		joined := strings.Join(command, "\n")
		for _, expected := range []string{"snowcat-cockpit", "lease-proxy", "worker-1234567890abcdef", workspace} {
			if !strings.Contains(joined, expected) {
				t.Errorf("%s command missing %q: %#v", provider, expected, command)
			}
		}
		if strings.Contains(joined, "SNOWCAT_MCP_TOKEN") || strings.Contains(joined, "https://snowcat") {
			t.Errorf("%s command contains upstream credential configuration: %#v", provider, command)
		}
		switch provider {
		case "codex":
			if !strings.Contains(joined, "mcp_servers.snowcat.enabled=false") {
				t.Errorf("Codex command did not disable direct Snowcat: %#v", command)
			}
		case "claude":
			if !slices.Contains(command, "--strict-mcp-config") || !strings.Contains(joined, `"type":"stdio"`) {
				t.Errorf("Claude command is not relay-only: %#v", command)
			}
		case "copilot":
			if !slices.Contains(command, "--disable-mcp-server") || !strings.Contains(joined, `"type":"local"`) {
				t.Errorf("Copilot command is not relay-only: %#v", command)
			}
		}
	}
}

func provisioningTestManager(t *testing.T, runner *fakeRunner) (*Manager, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	claudeHome := filepath.Join(root, "claude")
	ghConfig := filepath.Join(root, "gh")
	for _, path := range []string{filepath.Join(claudeHome, ".credentials.json"), filepath.Join(ghConfig, "hosts.yml"), filepath.Join(ghConfig, "config.yml")} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runner.source = source
	runner.baseCommit = strings.Repeat("c", 40)
	runner.remoteURL = "git@github.com:frostyard/std.git"
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: "node-provision-test",
		Runner: runner, Ready: func(string) error { return nil },
		LookPath: func(name string) (string, error) { return "/tools/" + name, nil },
		Random:   bytes.NewReader([]byte("1234567887654321")),
		Environment: func() []string {
			return []string{"PATH=/tools", "HOME=/home/test", "XDG_RUNTIME_DIR=/run/user/1000", "SNOWCAT_MCP_TOKEN=never-in-argv", "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "GH_TOKEN=github-never-in-argv"}
		},
		OCI: OCIConfig{Images: map[string]string{"claude": "sha256:" + strings.Repeat("e", 64)}, ClaudeHome: claudeHome, GHConfigDir: ghConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, source
}

func TestOCIWorkerProvisionsRepositoryToolsBeforeLaunch(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{pinFiles: map[string]string{"mise.toml": "[tools]\ngolangci-lint = \"2.13.1\"\n", "mise.lock": "lockfile_version = 1\n", "go.mod": "module x\n\ngo 1.26\n\ntoolchain go1.26.7\n"}}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "implementer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if record.Provisioning == nil || record.Provisioning.LockDigest == "" || !strings.HasPrefix(record.Provisioning.Cache, filepath.Join(manager.stateDirectory, "mise", "frostyard", "std")) {
		t.Fatalf("provisioning = %#v", record.Provisioning)
	}
	if want := []string{"go@1.26.7", "golangci-lint@2.13.1"}; strings.Join(record.Provisioning.Tools, ",") != strings.Join(want, ",") {
		t.Fatalf("tools = %v, want %v", record.Provisioning.Tools, want)
	}
	if _, err := os.Stat(filepath.Join(record.Provisioning.Cache, ".provisioned.json")); err != nil {
		t.Fatalf("cache marker: %v", err)
	}
	var provision, launch Command
	for _, command := range runner.commands {
		joined := strings.Join(command.Arguments, "\x00")
		if strings.Contains(joined, "MISE_LOCKED=1") {
			provision = command
		}
		if strings.Contains(joined, "new-session") {
			launch = command
		}
	}
	provisionArgs := strings.Join(provision.Arguments, "\n")
	for _, want := range []string{"--read-only", "--cap-drop=ALL", "--pids-limit=1024", "destination=/workspace,readonly", "destination=" + MiseDataDirectory + "\n", "--entrypoint\n/bin/sh", "mise install --locked"} {
		if !strings.Contains(provisionArgs+"\n", want) {
			t.Fatalf("provisioning container lacks %q: %v", want, provision.Arguments)
		}
	}
	for _, forbidden := range []string{"SNOWCAT_MCP_TOKEN", "GH_TOKEN", ".credentials.json"} {
		if strings.Contains(provisionArgs, forbidden) {
			t.Fatalf("provisioning container received %q: %v", forbidden, provision.Arguments)
		}
	}
	launchArgs := strings.Join(launch.Arguments, "\n")
	if !strings.Contains(launchArgs, "source="+record.Provisioning.Cache+",destination="+MiseDataDirectory+",readonly") {
		t.Fatalf("worker container does not mount the cache read-only: %v", launch.Arguments)
	}
	if runner.provisionRuns != 1 {
		t.Fatalf("provisioning ran %d times", runner.provisionRuns)
	}

	// The same pin files provision from the cache without a second container run.
	runner.commands = nil
	second, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "reviewer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if runner.provisionRuns != 1 || second.Provisioning == nil || second.Provisioning.Cache != record.Provisioning.Cache {
		t.Fatalf("cache reuse: runs=%d provisioning=%#v", runner.provisionRuns, second.Provisioning)
	}
}

func TestOCIWorkerWithoutPinFilesProvisionsNothing(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "discoverer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if record.Provisioning != nil || runner.provisionRuns != 0 {
		t.Fatalf("unexpected provisioning: %#v runs=%d", record.Provisioning, runner.provisionRuns)
	}
	for _, command := range runner.commands {
		if strings.Contains(strings.Join(command.Arguments, "\x00"), MiseDataDirectory) {
			t.Fatalf("worker container mounted a cache it has no reason to: %v", command.Arguments)
		}
	}
}

func TestOCIWorkerFailsWithTheToolNamedWhenTheLockCannotBeSatisfied(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{provisionFails: true, pinFiles: map[string]string{"mise.toml": "[tools]\njq = \"1.7.1\"\n", "mise.lock": "lockfile_version = 1\n"}}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "implementer", Repository: "frostyard/std", Source: source})
	if err == nil || !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "jq@1.7.1 is not in the lockfile") {
		t.Fatalf("err = %v", err)
	}
	if record.Status != StatusFailed || !strings.Contains(record.Detail, "repository tool provisioning failed") {
		t.Fatalf("record = %#v", record)
	}
	if entries, _ := os.ReadDir(filepath.Join(manager.stateDirectory, "mise", "frostyard", "std")); len(entries) != 0 {
		t.Fatalf("failed provisioning left a cache: %v", entries)
	}
	for _, command := range runner.commands {
		if strings.Contains(strings.Join(command.Arguments, "\x00"), "new-session") {
			t.Fatalf("a provider was launched after provisioning failed: %v", command.Arguments)
		}
	}
}

func TestOCIWorkerRefusesMiseTomlWithoutALock(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{pinFiles: map[string]string{"mise.toml": "[tools]\n"}}
	manager, source := provisioningTestManager(t, runner)
	_, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "implementer", Repository: "frostyard/std", Source: source})
	if err == nil || !errors.Is(err, ErrNotReady) || !strings.Contains(err.Error(), "mise.toml without mise.lock") {
		t.Fatalf("err = %v", err)
	}
	if runner.provisionRuns != 0 {
		t.Fatal("provisioning ran without a lock")
	}
}

func TestCleanupComparesOwnedSkillsAgainstTheWorkersRecordedKit(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "discoverer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if record.Kit == nil || record.Kit.Revision == "" || record.Kit.Skills["work-snowcat-queue"] == "" {
		t.Fatalf("launch did not record the kit: %#v", record.Kit)
	}
	if _, err := manager.Stop(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}

	// A worker launched under an older kit: its record names the digest of the
	// file it was given, so cleanup accepts that file even though the node's
	// current lock differs.
	older := []byte("# an earlier revision of the skill\n")
	for _, root := range []string{".agents", ".claude"} {
		if err := os.WriteFile(filepath.Join(record.Workspace, root, "skills", "work-snowcat-queue", "SKILL.md"), older, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(older)
	record.Kit.Skills["work-snowcat-queue"] = hex.EncodeToString(sum[:])
	if err := manager.write(record); err != nil {
		t.Fatal(err)
	}
	cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{})
	if err != nil || cleaned.Status != StatusCleaned || cleaned.Detail != "workspace cleaned; branch retained" {
		t.Fatalf("cleanup against the recorded kit: %#v, %v", cleaned, err)
	}
}

func TestCleanupRefusesDriftedOwnedSkillsUnlessDiscarded(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "discoverer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	skill := filepath.Join(record.Workspace, ".claude", "skills", "review-snowcat-queue", "SKILL.md")
	if err := os.WriteFile(skill, []byte("edited inside the lease\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{}); err == nil || !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "review-snowcat-queue") {
		t.Fatalf("drifted skill was not refused: %v", err)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("refused cleanup removed the workspace: %v", err)
	}
	cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{DiscardDriftedSkills: true})
	if err != nil || cleaned.Status != StatusCleaned || !strings.Contains(cleaned.Detail, "drifted and were discarded: .claude/skills/review-snowcat-queue") || !strings.HasSuffix(cleaned.Detail, "workspace cleaned; branch retained") {
		t.Fatalf("discarding cleanup: %#v, %v", cleaned, err)
	}
	if _, err := os.Stat(record.Workspace); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("workspace remains after discarding cleanup: %v", err)
	}
}

func TestCleanupOfALegacyRecordUsesTheCurrentLock(t *testing.T) {
	t.Parallel()

	runner := &fakeRunner{}
	manager, source := provisioningTestManager(t, runner)
	record, err := manager.Launch(context.Background(), LaunchRequest{Adapter: AdapterOCI, Provider: "claude", Role: "discoverer", Repository: "frostyard/std", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stop(context.Background(), record.ID); err != nil {
		t.Fatal(err)
	}
	record.Kit = nil
	if err := manager.write(record); err != nil {
		t.Fatal(err)
	}
	if cleaned, err := manager.Cleanup(context.Background(), record.ID, CleanupOptions{}); err != nil || cleaned.Status != StatusCleaned {
		t.Fatalf("legacy record cleanup: %#v, %v", cleaned, err)
	}
}

// fakeTTYDSource stands in for the real ttyd binary in OpenConsole tests. It
// accepts ttyd's own flags, then either exits nonzero before binding
// (FAKE_TTYD_MODE=fail, exercising an early-exit console) or binds the given
// loopback port and blocks (exercising a successful console).
const fakeTTYDSource = `package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	port := flag.String("p", "", "port")
	flag.String("i", "", "interface")
	flag.Bool("W", false, "")
	flag.Bool("O", false, "")
	flag.String("m", "", "")
	flag.Parse()
	if os.Getenv("FAKE_TTYD_MODE") == "fail" {
		fmt.Fprintln(os.Stderr, "fake ttyd: bind refused: address already in use")
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:"+*port)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer listener.Close()
	time.Sleep(10 * time.Second)
}
`

func buildFakeTTYD(t *testing.T) string {
	t.Helper()
	goPath, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain is not installed")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "fakettyd.go")
	if err := os.WriteFile(source, []byte(fakeTTYDSource), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "fakettyd")
	command := exec.Command(goPath, "build", "-o", binary, source)
	command.Env = os.Environ()
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build fake ttyd: %v: %s", err, output)
	}
	return binary
}

func openConsoleTestManager(t *testing.T, nodeID, ttydPath string, environment func() []string) (*Manager, Record) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("c", 40)}
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"), NodeID: nodeID,
		Runner: runner,
		LookPath: func(name string) (string, error) {
			switch name {
			case "tmux":
				return "/tools/tmux", nil
			case "ttyd":
				return ttydPath, nil
			default:
				return "/tools/" + name, nil
			}
		},
		Environment: func() []string {
			// Launch requires the worker MCP endpoint and token in the node
			// environment; supply test values regardless of the host's.
			return append(environment(), "SNOWCAT_MCP_URL=https://snowcat.invalid/mcp", "SNOWCAT_MCP_TOKEN=worker-secret")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := manager.Launch(context.Background(), LaunchRequest{Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: source})
	if err != nil {
		t.Fatal(err)
	}
	return manager, record
}

func TestOpenConsoleClassifiesEarlyTTYDExitAsNotReady(t *testing.T) {
	t.Parallel()

	ttydPath := buildFakeTTYD(t)
	manager, record := openConsoleTestManager(t, "node-console-not-ready", ttydPath, func() []string {
		return append(os.Environ(), "FAKE_TTYD_MODE=fail")
	})

	started := time.Now()
	_, err := manager.OpenConsole(context.Background(), record.ID)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("OpenConsole error = %v, want ErrNotReady", err)
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("OpenConsole error = %v, want the bounded ttyd diagnostic", err)
	}
	if elapsed >= 750*time.Millisecond {
		t.Fatalf("OpenConsole took %v, want to return promptly on an early ttyd exit", elapsed)
	}
	manager.mutex.Lock()
	_, tracked := manager.consoles[record.ID]
	manager.mutex.Unlock()
	if tracked {
		t.Fatalf("OpenConsole left a console process tracked after ttyd exited")
	}
}

func TestOpenConsoleSucceedsWhenTTYDBindsTheLoopbackPort(t *testing.T) {
	t.Parallel()

	ttydPath := buildFakeTTYD(t)
	manager, record := openConsoleTestManager(t, "node-console-ready", ttydPath, func() []string { return os.Environ() })
	t.Cleanup(func() {
		manager.mutex.Lock()
		manager.stopConsoleLocked(record.ID)
		manager.mutex.Unlock()
	})

	console, err := manager.OpenConsole(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if console.URL == "" {
		t.Fatalf("console URL is empty")
	}
	second, err := manager.OpenConsole(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.URL != console.URL {
		t.Fatalf("reopening an already-open console changed its URL: %s != %s", second.URL, console.URL)
	}
}
