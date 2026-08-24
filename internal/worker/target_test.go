package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type targetRunner struct {
	workspace      string
	apiHead        string
	fetchedHead    string
	headRef        string
	headRepository string
	currentBranch  string
	currentHead    string
	commands       []Command
}

func (runner *targetRunner) Run(_ context.Context, command Command) ([]byte, error) {
	runner.commands = append(runner.commands, command)
	arguments := strings.Join(command.Arguments, "\x00")
	switch {
	case arguments == "rev-parse\x00--show-toplevel":
		return []byte(runner.workspace + "\n"), nil
	case arguments == "status\x00--porcelain":
		return nil, nil
	case arguments == "branch\x00--show-current":
		return []byte(runner.currentBranch + "\n"), nil
	case strings.HasPrefix(arguments, "api\x00repos/"):
		payload, err := json.Marshal(map[string]any{"head": map[string]any{
			"sha": runner.apiHead, "ref": runner.headRef, "repo": map[string]string{"full_name": runner.headRepository},
		}})
		return payload, err
	case strings.HasPrefix(arguments, "check-ref-format\x00--branch\x00"):
		return nil, nil
	case strings.HasPrefix(arguments, "fetch\x00--no-tags\x00--force"):
		return nil, nil
	case strings.HasPrefix(arguments, "rev-parse\x00--verify\x00refs/cockpit/targets/"):
		return []byte(runner.fetchedHead + "\n"), nil
	case strings.HasPrefix(arguments, "checkout\x00--detach"):
		runner.currentBranch = ""
		runner.currentHead = command.Arguments[len(command.Arguments)-1]
		return nil, nil
	case strings.HasPrefix(arguments, "checkout\x00-B"):
		runner.currentBranch = command.Arguments[2]
		runner.currentHead = command.Arguments[3]
		return nil, nil
	case arguments == "rev-parse\x00HEAD^{commit}":
		return []byte(runner.currentHead + "\n"), nil
	case strings.HasPrefix(arguments, "merge-base\x00--is-ancestor"):
		return nil, nil
	case strings.HasPrefix(arguments, "push\x00--force-with-lease="):
		runner.apiHead = runner.currentHead
		return nil, nil
	default:
		return nil, errors.New("unexpected target command")
	}
}

func TestPrepareAndPushBoundPullRequestTarget(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	workerID := "worker-0123456789abcdef"
	boundHead := strings.Repeat("a", 40)
	pushedHead := strings.Repeat("b", 40)
	runner := &targetRunner{
		workspace: workspace, apiHead: boundHead, fetchedHead: boundHead,
		headRef: "feature/bound-fix", headRepository: "frostyard/firn",
		currentBranch: "cockpit/" + workerID, currentHead: strings.Repeat("c", 40),
	}
	lookPath := func(name string) (string, error) { return "/tools/" + name, nil }
	target, err := PrepareTarget(context.Background(), TargetRequest{
		WorkerID: workerID, Repository: "frostyard/firn",
		ItemID: "01234567-89ab-cdef-0123-456789abcdef", Kind: "pr-review-fix",
		PullRequestURL: "https://github.com/frostyard/firn/pull/42", HeadSHA: boundHead,
	}, workspace, runner, lookPath)
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode != TargetModeBranch || target.TargetBranch != "feature/bound-fix" || target.BoundHead != boundHead {
		t.Fatalf("target = %#v", target)
	}
	runner.currentHead = pushedHead
	result, err := PushTarget(context.Background(), workerID, workspace, runner, lookPath)
	if err != nil {
		t.Fatal(err)
	}
	if result.PreviousHead != boundHead || result.PushedHead != pushedHead {
		t.Fatalf("push = %#v", result)
	}
	stored, exists, err := readTarget(workspace)
	if err != nil || !exists || stored.LeaseHead != pushedHead {
		t.Fatalf("stored target = %#v, %v, %v", stored, exists, err)
	}
	var fetch Command
	var push Command
	for _, command := range runner.commands {
		if slices.Contains(command.Arguments, "fetch") {
			fetch = command
		}
		if slices.Contains(command.Arguments, "push") {
			push = command
		}
	}
	if !slices.Contains(fetch.Arguments, "https://github.com/frostyard/firn.git") || !slices.Contains(fetch.Arguments, "+refs/heads/feature/bound-fix:refs/cockpit/targets/"+workerID) {
		t.Fatalf("fetch = %#v", fetch.Arguments)
	}
	if !slices.Contains(push.Arguments, "--force-with-lease=refs/heads/feature/bound-fix:"+boundHead) || !slices.Contains(push.Arguments, "HEAD:refs/heads/feature/bound-fix") {
		t.Fatalf("push = %#v", push.Arguments)
	}
}

func TestPrepareTargetRefusesMovedHeadBeforeFetch(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	workerID := "worker-fedcba9876543210"
	runner := &targetRunner{
		workspace: workspace, apiHead: strings.Repeat("b", 40), fetchedHead: strings.Repeat("b", 40),
		headRef: "feature/moved", headRepository: "frostyard/firn", currentBranch: "cockpit/" + workerID,
	}
	_, err := PrepareTarget(context.Background(), TargetRequest{
		WorkerID: workerID, Repository: "frostyard/firn",
		ItemID: "fedcba98-7654-3210-fedc-ba9876543210", Kind: "pr-cure",
		PullRequestURL: "https://github.com/frostyard/firn/pull/7", HeadSHA: strings.Repeat("a", 40),
	}, workspace, runner, func(name string) (string, error) { return "/tools/" + name, nil })
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "head moved") {
		t.Fatalf("error = %v", err)
	}
	for _, command := range runner.commands {
		if slices.Contains(command.Arguments, "fetch") {
			t.Fatalf("moved head was fetched: %#v", command.Arguments)
		}
	}
}

func TestPrepareReviewTargetChecksOutDetachedExactHead(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	workerID := "worker-0011223344556677"
	head := strings.Repeat("d", 40)
	runner := &targetRunner{
		workspace: workspace, apiHead: head, fetchedHead: head, headRef: "review/head",
		headRepository: "frostyard/firn", currentBranch: "cockpit/" + workerID,
	}
	target, err := PrepareTarget(context.Background(), TargetRequest{
		WorkerID: workerID, Repository: "frostyard/firn",
		ItemID: "00112233-4455-6677-8899-aabbccddeeff", Kind: "pr-review",
		PullRequestURL: "https://github.com/frostyard/firn/pull/9", HeadSHA: head,
	}, workspace, runner, func(name string) (string, error) { return "/tools/" + name, nil })
	if err != nil {
		t.Fatal(err)
	}
	if target.Mode != TargetModeDetached || target.LocalBranch != "" || runner.currentBranch != "" || runner.currentHead != head {
		t.Fatalf("target = %#v; runner = %#v", target, runner)
	}
}
