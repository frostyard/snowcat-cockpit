package managedrepo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid managed repository")
	ErrNotFound = errors.New("managed repository not found")
	ErrConflict = errors.New("managed repository conflict")

	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	branchRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	commitRE     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

const (
	StatusPending = "pending"
	StatusReady   = "ready"
	StatusFailed  = "failed"
)

type Record struct {
	Repository string     `json:"repository"`
	Source     string     `json:"source"`
	BaseRef    string     `json:"baseRef"`
	BaseCommit string     `json:"baseCommit,omitempty"`
	Status     string     `json:"status"`
	Detail     string     `json:"detail"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	PreparedAt *time.Time `json:"preparedAt,omitempty"`
}

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).Output()
}

type Catalog struct {
	statePath  string
	sourceRoot string
	runner     Runner
	now        func() time.Time
	mu         sync.Mutex
}

func New(stateDirectory, sourceRoot string) (*Catalog, error) {
	return NewWithRunner(stateDirectory, sourceRoot, OSRunner{}, time.Now)
}

func NewWithRunner(stateDirectory, sourceRoot string, runner Runner, now func() time.Time) (*Catalog, error) {
	if stateDirectory == "" || sourceRoot == "" || runner == nil || now == nil {
		return nil, fmt.Errorf("%w: state directory, source root, runner, and clock are required", ErrInvalid)
	}
	stateDirectory, err := filepath.Abs(stateDirectory)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve state directory", ErrInvalid)
	}
	sourceRoot, err = filepath.Abs(sourceRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve source root", ErrInvalid)
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("open managed repository state: %w", err)
	}
	if err := os.Chmod(stateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("secure managed repository state: %w", err)
	}
	return &Catalog{
		statePath:  filepath.Join(stateDirectory, "repositories.json"),
		sourceRoot: sourceRoot,
		runner:     runner,
		now:        now,
	}, nil
}

func (catalog *Catalog) Enroll(_ context.Context, repository string) (Record, error) {
	repository = normalize(repository)
	if !repositoryRE.MatchString(repository) {
		return Record{}, fmt.Errorf("%w: repository must be owner/name", ErrInvalid)
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()

	records, err := catalog.readLocked()
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if strings.EqualFold(record.Repository, repository) {
			return record, nil
		}
	}
	now := catalog.now().UTC()
	parts := strings.Split(repository, "/")
	record := Record{
		Repository: repository,
		Source:     filepath.Join(catalog.sourceRoot, parts[0], parts[1]),
		BaseRef:    "origin/HEAD",
		Status:     StatusPending,
		Detail:     "managed source has not been prepared",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	records = append(records, record)
	if err := catalog.writeLocked(records); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (catalog *Catalog) List(_ context.Context) ([]Record, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	records, err := catalog.readLocked()
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Repository < records[j].Repository })
	return records, nil
}

func (catalog *Catalog) Setup(ctx context.Context, repository string) (Record, error) {
	repository = normalize(repository)
	catalog.mu.Lock()
	defer catalog.mu.Unlock()

	records, err := catalog.readLocked()
	if err != nil {
		return Record{}, err
	}
	index := -1
	for candidate := range records {
		if strings.EqualFold(records[candidate].Repository, repository) {
			index = candidate
			break
		}
	}
	if index < 0 {
		return Record{}, ErrNotFound
	}
	record := records[index]
	prepared, setupErr := catalog.prepare(ctx, record)
	prepared.UpdatedAt = catalog.now().UTC()
	if setupErr != nil {
		prepared.Status = StatusFailed
		prepared.Detail = setupErr.Error()
		prepared.BaseCommit = ""
		prepared.PreparedAt = nil
	}
	records[index] = prepared
	if err := catalog.writeLocked(records); err != nil {
		return Record{}, err
	}
	if setupErr != nil {
		return prepared, setupErr
	}
	return prepared, nil
}

func (catalog *Catalog) prepare(ctx context.Context, record Record) (Record, error) {
	info, err := os.Lstat(record.Source)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(record.Source), 0o700); err != nil {
			return record, fmt.Errorf("create managed source parent: %w", err)
		}
		if _, err := catalog.runner.Run(ctx, "gh", "repo", "clone", record.Repository, record.Source, "--", "--origin", "origin"); err != nil {
			return record, fmt.Errorf("clone managed source with GitHub CLI failed")
		}
	case err != nil:
		return record, fmt.Errorf("inspect managed source: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return record, fmt.Errorf("%w: managed source path must be a real directory", ErrConflict)
	}

	origin, err := catalog.runner.Run(ctx, "git", "-C", record.Source, "remote", "get-url", "origin")
	if err != nil || !matchesGitHubRepository(strings.TrimSpace(string(origin)), record.Repository) {
		return record, fmt.Errorf("%w: managed source origin does not match repository", ErrConflict)
	}
	status, err := catalog.runner.Run(ctx, "git", "-C", record.Source, "status", "--porcelain=v1")
	if err != nil {
		return record, fmt.Errorf("inspect managed source worktree failed")
	}
	if len(bytesTrimSpace(status)) != 0 {
		return record, fmt.Errorf("%w: managed source worktree is not clean", ErrConflict)
	}
	if _, err := catalog.runner.Run(ctx, "git", "-C", record.Source, "fetch", "--prune", "origin"); err != nil {
		return record, fmt.Errorf("refresh managed source failed")
	}

	baseRef := ""
	if output, err := catalog.runner.Run(ctx, "git", "-C", record.Source, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		baseRef = strings.TrimSpace(string(output))
	}
	if baseRef == "" {
		output, err := catalog.runner.Run(ctx, "gh", "repo", "view", record.Repository, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
		branch := strings.TrimSpace(string(output))
		if err != nil || !branchRE.MatchString(branch) || strings.Contains(branch, "..") {
			return record, fmt.Errorf("resolve managed source default branch failed")
		}
		baseRef = "origin/" + branch
	}
	if !branchRE.MatchString(baseRef) || strings.Contains(baseRef, "..") {
		return record, fmt.Errorf("resolve managed source default branch failed")
	}
	commit, err := catalog.runner.Run(ctx, "git", "-C", record.Source, "rev-parse", "--verify", baseRef+"^{commit}")
	baseCommit := strings.TrimSpace(string(commit))
	if err != nil || !commitRE.MatchString(baseCommit) {
		return record, fmt.Errorf("resolve managed source base commit failed")
	}
	preparedAt := catalog.now().UTC()
	record.BaseRef = baseRef
	record.BaseCommit = baseCommit
	record.Status = StatusReady
	record.Detail = "managed source refreshed from credential-free GitHub origin"
	record.PreparedAt = &preparedAt
	return record, nil
}

func (catalog *Catalog) readLocked() ([]Record, error) {
	content, err := os.ReadFile(catalog.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read managed repository state: %w", err)
	}
	var records []Record
	if err := json.Unmarshal(content, &records); err != nil {
		return nil, fmt.Errorf("decode managed repository state: %w", err)
	}
	return records, nil
}

func (catalog *Catalog) writeLocked(records []Record) error {
	content, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode managed repository state: %w", err)
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(catalog.statePath), ".repositories-*.json")
	if err != nil {
		return fmt.Errorf("create managed repository state: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() { _ = os.Remove(temporaryName) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure managed repository state: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write managed repository state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync managed repository state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close managed repository state: %w", err)
	}
	if err := os.Rename(temporaryName, catalog.statePath); err != nil {
		return fmt.Errorf("replace managed repository state: %w", err)
	}
	return nil
}

func normalize(repository string) string {
	parts := strings.Split(strings.TrimSpace(repository), "/")
	if len(parts) != 2 {
		return strings.TrimSpace(repository)
	}
	return strings.ToLower(parts[0]) + "/" + strings.ToLower(parts[1])
}

func matchesGitHubRepository(origin, repository string) bool {
	if strings.Contains(origin, "@") && strings.HasPrefix(origin, "https://") {
		return false
	}
	wanted := strings.ToLower(strings.TrimSuffix(repository, ".git"))
	candidates := []string{
		"https://github.com/" + wanted,
		"ssh://git@github.com/" + wanted,
		"git@github.com:" + wanted,
	}
	origin = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(origin), ".git"))
	for _, candidate := range candidates {
		if origin == candidate {
			return true
		}
	}
	return false
}

func bytesTrimSpace(value []byte) []byte {
	return []byte(strings.TrimSpace(string(value)))
}
