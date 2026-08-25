package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	TargetModeBranch   = "branch"
	TargetModeDetached = "detached"
	targetVersion      = 1
	targetMarker       = "cockpit-target.json"
)

var (
	workItemIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	headSHARE    = regexp.MustCompile(`^[0-9a-f]{40}$`)
	headRefRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,239}$`)
)

type TargetRequest struct {
	WorkerID       string `json:"workerId"`
	Repository     string `json:"repository"`
	ItemID         string `json:"itemId"`
	Kind           string `json:"kind"`
	PullRequestURL string `json:"pullRequestUrl"`
	HeadSHA        string `json:"headSha"`
}

type Target struct {
	Version          int    `json:"version"`
	WorkerID         string `json:"workerId"`
	Repository       string `json:"repository"`
	ItemID           string `json:"itemId"`
	Kind             string `json:"kind"`
	PullRequestURL   string `json:"pullRequestUrl"`
	BoundHead        string `json:"boundHead"`
	LeaseHead        string `json:"leaseHead"`
	TargetRepository string `json:"targetRepository"`
	TargetBranch     string `json:"targetBranch"`
	LocalBranch      string `json:"localBranch,omitempty"`
	Mode             string `json:"mode"`
}

type PushResult struct {
	PullRequestURL string `json:"pullRequestUrl"`
	TargetBranch   string `json:"targetBranch"`
	PreviousHead   string `json:"previousHead"`
	PushedHead     string `json:"pushedHead"`
}

type pullRequestProjection struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func PrepareTarget(ctx context.Context, request TargetRequest, directory string, runner Runner, lookPath func(string) (string, error)) (Target, error) {
	mode, err := targetMode(request.Kind)
	if err != nil {
		return Target{}, err
	}
	request.HeadSHA = strings.ToLower(request.HeadSHA)
	pullNumber, canonicalURL, err := validateTargetRequest(request)
	if err != nil {
		return Target{}, err
	}
	workspace, gitPath, ghPath, err := targetTools(ctx, directory, runner, lookPath)
	if err != nil {
		return Target{}, err
	}
	if _, exists, err := readTarget(workspace); err != nil {
		return Target{}, err
	} else if exists {
		return Target{}, fmt.Errorf("%w: worker target is already prepared", ErrConflict)
	}
	if output, err := runCommand(ctx, runner, gitPath, workspace, "status", "--porcelain"); err != nil {
		return Target{}, fmt.Errorf("inspect target workspace: %w", err)
	} else if len(strings.TrimSpace(string(output))) != 0 {
		return Target{}, fmt.Errorf("%w: target workspace is not clean", ErrConflict)
	}
	localBranch := "cockpit/" + request.WorkerID
	if output, err := runCommand(ctx, runner, gitPath, workspace, "branch", "--show-current"); err != nil {
		return Target{}, fmt.Errorf("inspect target workspace branch: %w", err)
	} else if strings.TrimSpace(string(output)) != localBranch {
		return Target{}, fmt.Errorf("%w: target workspace is not on its Cockpit branch", ErrConflict)
	}

	pull, err := inspectPullRequest(ctx, runner, ghPath, workspace, request.Repository, pullNumber)
	if err != nil {
		return Target{}, err
	}
	if pull.Merged {
		return Target{}, fmt.Errorf("%w: bound pull request is merged; nothing can be delivered and its head branch may no longer exist", ErrConflict)
	}
	if state := strings.ToLower(pull.State); state != "open" {
		return Target{}, fmt.Errorf("%w: bound pull request is %s; nothing can be delivered", ErrConflict, state)
	}
	if strings.ToLower(pull.Head.SHA) != request.HeadSHA {
		return Target{}, fmt.Errorf("%w: pull-request head moved from %s to %s", ErrConflict, shortHead(request.HeadSHA), shortHead(pull.Head.SHA))
	}
	if pull.Head.Repo == nil || !repositoryRE.MatchString(pull.Head.Repo.FullName) {
		return Target{}, fmt.Errorf("%w: GitHub returned an invalid pull-request head repository", ErrInvalid)
	}
	if err := validateHeadRef(pull.Head.Ref); err != nil {
		return Target{}, err
	}
	if _, err := runCommand(ctx, runner, gitPath, workspace, "check-ref-format", "--branch", pull.Head.Ref); err != nil {
		return Target{}, fmt.Errorf("%w: GitHub returned an invalid pull-request head branch", ErrInvalid)
	}
	targetRepository := pull.Head.Repo.FullName
	targetURL := "https://github.com/" + targetRepository + ".git"
	fetchedRef := "refs/cockpit/targets/" + request.WorkerID
	refspec := "+refs/heads/" + pull.Head.Ref + ":" + fetchedRef
	if output, err := runCommand(ctx, runner, gitPath, workspace, "fetch", "--no-tags", "--force", targetURL, refspec); err != nil {
		if detail := commandDetail(output); detail != "" {
			return Target{}, fmt.Errorf("fetch bound pull-request head %s from %s: %w: %s", pull.Head.Ref, targetRepository, err, detail)
		}
		return Target{}, fmt.Errorf("fetch bound pull-request head %s from %s: %w", pull.Head.Ref, targetRepository, err)
	}
	output, err := runCommand(ctx, runner, gitPath, workspace, "rev-parse", "--verify", fetchedRef+"^{commit}")
	if err != nil {
		return Target{}, fmt.Errorf("resolve fetched pull-request head: %w", err)
	}
	if fetched := strings.ToLower(strings.TrimSpace(string(output))); fetched != request.HeadSHA {
		return Target{}, fmt.Errorf("%w: pull-request head moved while it was fetched", ErrConflict)
	}
	if mode == TargetModeDetached {
		if _, err := runCommand(ctx, runner, gitPath, workspace, "checkout", "--detach", request.HeadSHA); err != nil {
			return Target{}, fmt.Errorf("check out exact review head: %w", err)
		}
		localBranch = ""
	} else if _, err := runCommand(ctx, runner, gitPath, workspace, "checkout", "-B", localBranch, request.HeadSHA); err != nil {
		return Target{}, fmt.Errorf("check out bound pull-request head: %w", err)
	}
	target := Target{
		Version: targetVersion, WorkerID: request.WorkerID, Repository: request.Repository,
		ItemID: request.ItemID, Kind: request.Kind, PullRequestURL: canonicalURL,
		BoundHead: request.HeadSHA, LeaseHead: request.HeadSHA,
		TargetRepository: targetRepository, TargetBranch: pull.Head.Ref,
		LocalBranch: localBranch, Mode: mode,
	}
	if err := writeTarget(workspace, target); err != nil {
		return Target{}, err
	}
	return target, nil
}

func PushTarget(ctx context.Context, workerID, directory string, runner Runner, lookPath func(string) (string, error)) (PushResult, error) {
	if !workerIDRE.MatchString(workerID) {
		return PushResult{}, fmt.Errorf("%w: invalid worker ID", ErrInvalid)
	}
	workspace, gitPath, ghPath, err := targetTools(ctx, directory, runner, lookPath)
	if err != nil {
		return PushResult{}, err
	}
	target, exists, err := readTarget(workspace)
	if err != nil {
		return PushResult{}, err
	}
	if !exists {
		return PushResult{}, fmt.Errorf("%w: worker target is not prepared", ErrConflict)
	}
	if err := validateTarget(target); err != nil {
		return PushResult{}, err
	}
	if target.WorkerID != workerID || target.Mode != TargetModeBranch {
		return PushResult{}, fmt.Errorf("%w: worker target is not a writable branch", ErrConflict)
	}
	if output, err := runCommand(ctx, runner, gitPath, workspace, "branch", "--show-current"); err != nil {
		return PushResult{}, fmt.Errorf("inspect target workspace branch: %w", err)
	} else if strings.TrimSpace(string(output)) != target.LocalBranch {
		return PushResult{}, fmt.Errorf("%w: worker left its prepared target branch", ErrConflict)
	}
	output, err := runCommand(ctx, runner, gitPath, workspace, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return PushResult{}, fmt.Errorf("resolve target workspace head: %w", err)
	}
	currentHead := strings.ToLower(strings.TrimSpace(string(output)))
	if !headSHARE.MatchString(currentHead) {
		return PushResult{}, fmt.Errorf("%w: Git returned an invalid workspace head", ErrInvalid)
	}
	if _, err := runCommand(ctx, runner, gitPath, workspace, "merge-base", "--is-ancestor", target.BoundHead, currentHead); err != nil {
		return PushResult{}, fmt.Errorf("%w: workspace head does not descend from the bound pull-request head", ErrConflict)
	}
	pullNumber, _, err := parsePullRequestURL(target.PullRequestURL, target.Repository)
	if err != nil {
		return PushResult{}, err
	}
	pull, err := inspectPullRequest(ctx, runner, ghPath, workspace, target.Repository, pullNumber)
	if err != nil {
		return PushResult{}, err
	}
	if strings.ToLower(pull.Head.SHA) != target.LeaseHead {
		return PushResult{}, fmt.Errorf("%w: pull-request head moved from %s to %s", ErrConflict, shortHead(target.LeaseHead), shortHead(pull.Head.SHA))
	}
	if pull.Head.Repo == nil || pull.Head.Repo.FullName != target.TargetRepository || pull.Head.Ref != target.TargetBranch {
		return PushResult{}, fmt.Errorf("%w: pull-request target changed after preparation", ErrConflict)
	}
	targetURL := "https://github.com/" + target.TargetRepository + ".git"
	lease := "--force-with-lease=refs/heads/" + target.TargetBranch + ":" + target.LeaseHead
	refspec := "HEAD:refs/heads/" + target.TargetBranch
	if _, err := runCommand(ctx, runner, gitPath, workspace, "push", lease, targetURL, refspec); err != nil {
		return PushResult{}, fmt.Errorf("push bound pull-request branch: %w", err)
	}
	result := PushResult{
		PullRequestURL: target.PullRequestURL, TargetBranch: target.TargetBranch,
		PreviousHead: target.LeaseHead, PushedHead: currentHead,
	}
	target.LeaseHead = currentHead
	if err := writeTarget(workspace, target); err != nil {
		return PushResult{}, err
	}
	return result, nil
}

func targetTools(ctx context.Context, directory string, runner Runner, lookPath func(string) (string, error)) (string, string, string, error) {
	if runner == nil || lookPath == nil {
		return "", "", "", fmt.Errorf("%w: target runner and executable lookup are required", ErrInvalid)
	}
	gitPath, err := lookPath("git")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: git is not available", ErrNotReady)
	}
	ghPath, err := lookPath("gh")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: gh is not available", ErrNotReady)
	}
	resolved, err := canonicalDirectory(directory)
	if err != nil {
		return "", "", "", fmt.Errorf("%w: target workspace: %v", ErrInvalid, err)
	}
	output, err := runCommand(ctx, runner, gitPath, resolved, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", "", fmt.Errorf("%w: target workspace is not a Git working tree", ErrInvalid)
	}
	workspace, err := canonicalDirectory(strings.TrimSpace(string(output)))
	if err != nil || workspace != resolved {
		return "", "", "", fmt.Errorf("%w: command must run from the worker workspace root", ErrInvalid)
	}
	return workspace, gitPath, ghPath, nil
}

func inspectPullRequest(ctx context.Context, runner Runner, ghPath, workspace, repository string, number int) (pullRequestProjection, error) {
	output, err := runCommand(ctx, runner, ghPath, workspace,
		"api", "repos/"+repository+"/pulls/"+strconv.Itoa(number),
		"--jq", `{state:.state,merged:.merged,head:{sha:.head.sha,ref:.head.ref,repo:{full_name:.head.repo.full_name}}}`,
	)
	if err != nil {
		return pullRequestProjection{}, fmt.Errorf("inspect bound pull request: %w", err)
	}
	var pull pullRequestProjection
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(string(output)), maxOutput))
	if err := decoder.Decode(&pull); err != nil {
		return pullRequestProjection{}, fmt.Errorf("decode bound pull request: %w", err)
	}
	if !headSHARE.MatchString(strings.ToLower(pull.Head.SHA)) {
		return pullRequestProjection{}, fmt.Errorf("%w: GitHub returned an invalid pull-request head", ErrInvalid)
	}
	return pull, nil
}

func validateTargetRequest(request TargetRequest) (int, string, error) {
	if !workerIDRE.MatchString(request.WorkerID) || !repositoryRE.MatchString(request.Repository) || !workItemIDRE.MatchString(request.ItemID) || !headSHARE.MatchString(request.HeadSHA) {
		return 0, "", fmt.Errorf("%w: worker, repository, item, and 40-hex head are required", ErrInvalid)
	}
	number, canonicalURL, err := parsePullRequestURL(request.PullRequestURL, request.Repository)
	if err != nil {
		return 0, "", err
	}
	return number, canonicalURL, nil
}

func parsePullRequestURL(raw, repository string) (int, string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Host, "github.com") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return 0, "", fmt.Errorf("%w: pull request must be a credential-free github.com HTTPS URL", ErrInvalid)
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 4 || !strings.EqualFold(parts[0]+"/"+parts[1], repository) || parts[2] != "pull" {
		return 0, "", fmt.Errorf("%w: pull request does not match the worker repository", ErrInvalid)
	}
	number, err := strconv.Atoi(parts[3])
	if err != nil || number < 1 {
		return 0, "", fmt.Errorf("%w: pull request number is invalid", ErrInvalid)
	}
	return number, "https://github.com/" + repository + "/pull/" + strconv.Itoa(number), nil
}

func targetMode(kind string) (string, error) {
	switch kind {
	case "pr-review":
		return TargetModeDetached, nil
	case "pr-cure", "pr-cure-change", "pr-review-fix":
		return TargetModeBranch, nil
	default:
		return "", fmt.Errorf("%w: kind is not bound to an existing pull request", ErrInvalid)
	}
}

func validateHeadRef(ref string) error {
	if !headRefRE.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") {
		return fmt.Errorf("%w: GitHub returned an unsafe pull-request head branch", ErrInvalid)
	}
	for _, part := range strings.Split(ref, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("%w: GitHub returned an unsafe pull-request head branch", ErrInvalid)
		}
	}
	return nil
}

func validateTarget(target Target) error {
	if target.Version != targetVersion || !workerIDRE.MatchString(target.WorkerID) || !repositoryRE.MatchString(target.Repository) || !workItemIDRE.MatchString(target.ItemID) {
		return errors.New("decode worker target: invalid identity or version")
	}
	mode, err := targetMode(target.Kind)
	if err != nil || mode != target.Mode {
		return errors.New("decode worker target: invalid kind or mode")
	}
	if _, _, err := parsePullRequestURL(target.PullRequestURL, target.Repository); err != nil {
		return fmt.Errorf("decode worker target: %w", err)
	}
	if !headSHARE.MatchString(target.BoundHead) || !headSHARE.MatchString(target.LeaseHead) || !repositoryRE.MatchString(target.TargetRepository) || validateHeadRef(target.TargetBranch) != nil {
		return errors.New("decode worker target: invalid pull-request binding")
	}
	if target.Mode == TargetModeBranch && target.LocalBranch != "cockpit/"+target.WorkerID {
		return errors.New("decode worker target: invalid local branch")
	}
	if target.Mode == TargetModeDetached && target.LocalBranch != "" {
		return errors.New("decode worker target: detached target has a local branch")
	}
	return nil
}

func targetPath(workspace string) string {
	return filepath.Join(workspace, ".agents", targetMarker)
}

func readTarget(workspace string) (Target, bool, error) {
	path := targetPath(workspace)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, fmt.Errorf("inspect worker target: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o077 != 0 {
		return Target{}, false, errors.New("decode worker target: target must be a private regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Target{}, false, fmt.Errorf("read worker target: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxOutput))
	decoder.DisallowUnknownFields()
	var target Target
	if err := decoder.Decode(&target); err != nil {
		return Target{}, false, fmt.Errorf("decode worker target: %w", err)
	}
	if err := validateTarget(target); err != nil {
		return Target{}, false, err
	}
	return target, true, nil
}

func writeTarget(workspace string, target Target) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	directory := filepath.Dir(targetPath(workspace))
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("write worker target: private .agents directory is unavailable")
	}
	temporary, err := os.CreateTemp(directory, ".cockpit-target-*.json")
	if err != nil {
		return fmt.Errorf("create temporary worker target: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary worker target: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(target); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode worker target: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync worker target: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close worker target: %w", err)
	}
	if err := os.Rename(temporaryPath, targetPath(workspace)); err != nil {
		return fmt.Errorf("install worker target: %w", err)
	}
	return nil
}

func runCommand(ctx context.Context, runner Runner, name, directory string, arguments ...string) ([]byte, error) {
	return runner.Run(ctx, Command{Name: name, Arguments: arguments, Directory: directory, Env: os.Environ()})
}

// commandDetail returns the last non-empty output line of a failed Git command,
// bounded so a worker sees why a fetch failed without a wall of output.
func commandDetail(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" {
			continue
		}
		if len(line) > 200 {
			line = line[:200]
		}
		return line
	}
	return ""
}

func shortHead(head string) string {
	if len(head) < 12 {
		return head
	}
	return head[:12]
}
