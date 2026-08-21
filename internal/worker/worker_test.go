package worker

import (
	"bytes"
	"context"
	"errors"
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
	source     string
	dead       bool
	dirty      bool
	session    bool
	commands   []Command
	workspace  string
	baseCommit string
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
	cleaned, err := manager.Cleanup(context.Background(), record.ID)
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
	case strings.Contains(arguments, "rev-parse\x00--verify"):
		return []byte(runner.baseCommit + "\n"), nil
	case strings.Contains(arguments, "worktree\x00add"):
		runner.workspace = command.Arguments[len(command.Arguments)-2]
		if err := os.MkdirAll(runner.workspace, 0o700); err != nil {
			return nil, err
		}
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
	runner := &fakeRunner{source: source, baseCommit: strings.Repeat("a", 40)}
	now := time.Date(2026, 8, 21, 17, 30, 0, 0, time.UTC)
	manager, err := New(Config{
		StateDirectory: filepath.Join(root, "state"),
		NodeID:         "node-0123456789abcdef0123456789abcdef",
		Runner:         runner,
		Ready:          func(string) error { return nil },
		LookPath:       func(name string) (string, error) { return "/tools/" + name, nil },
		Now:            func() time.Time { return now },
		Random:         bytes.NewReader([]byte("12345678")),
		Environment:    func() []string { return []string{"PATH=/tools", "SECRET_SENTINEL=never-persist"} },
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
	joined := strings.Join(launchCommand.Arguments, "\n")
	if !strings.Contains(joined, "work-snowcat-queue") || !strings.Contains(joined, record.ID) || !strings.Contains(joined, "-fix") {
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
	if _, err := manager.Cleanup(context.Background(), record.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("dirty cleanup error = %v", err)
	}
	if _, err := os.Stat(record.Workspace); err != nil {
		t.Fatalf("dirty workspace was removed: %v", err)
	}
	runner.dirty = false
	cleaned, err := manager.Cleanup(context.Background(), record.ID)
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
		Random: bytes.NewReader([]byte("abcdefgh")), Environment: func() []string { return []string{"PATH=/tools"} },
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

func TestBuildPromptPinsRoleSelections(t *testing.T) {
	t.Parallel()

	discoverer := BuildPrompt("worker-1234567890abcdef", "discoverer", "frostyard/firn")
	for _, expected := range []string{"work-snowcat-queue", "only kinds ending in -discovery", "read-only discovery", "requiredArtifact", "open-pr", "at most one"} {
		if !strings.Contains(discoverer, expected) {
			t.Fatalf("discoverer prompt missing %q: %s", expected, discoverer)
		}
	}
	implementer := BuildPrompt("worker-1234567890abcdef", "implementer", "frostyard/firn")
	for _, expected := range []string{"work-snowcat-queue", "-fix", "pr-cure", "require open-pr", "release the item immediately", "pull-request", "at most one"} {
		if !strings.Contains(implementer, expected) {
			t.Fatalf("implementer prompt missing %q: %s", expected, implementer)
		}
	}
	reviewer := BuildPrompt("worker-1234567890abcdef", "reviewer", "frostyard/firn")
	if !strings.Contains(reviewer, "review-snowcat-queue") || !strings.Contains(reviewer, "only pr-review") {
		t.Fatalf("reviewer prompt = %s", reviewer)
	}
}
