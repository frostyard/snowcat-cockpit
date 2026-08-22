package managedrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeRunner struct {
	responses map[string][]byte
	errors    map[string]error
	calls     []string
}

func (runner *fakeRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	call := strings.Join(append([]string{name}, arguments...), " ")
	runner.calls = append(runner.calls, call)
	return runner.responses[call], runner.errors[call]
}

func TestEnrollIsIdempotentAndPersistsPrivateState(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 8, 21, 22, 0, 0, 0, time.UTC)
	catalog, err := NewWithRunner(filepath.Join(root, "state"), filepath.Join(root, "sources"), &fakeRunner{}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.Enroll(context.Background(), "Frostyard/Updex")
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Enroll(context.Background(), "frostyard/updex")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("idempotent enrollment changed record:\n%#v\n%#v", first, second)
	}
	if first.Repository != "frostyard/updex" || first.Status != StatusPending || first.BaseRef != "origin/HEAD" {
		t.Fatalf("unexpected record: %#v", first)
	}
	info, err := os.Stat(filepath.Join(root, "state", "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}
}

func TestSetupRefreshesExistingManagedSourceAndPinsCommit(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources", "frostyard", "updex")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 22, 0, 0, 0, time.UTC)
	runner := &fakeRunner{responses: map[string][]byte{}, errors: map[string]error{}}
	runner.responses["git -C "+source+" remote get-url origin"] = []byte("https://github.com/frostyard/updex.git\n")
	runner.responses["git -C "+source+" status --porcelain=v1"] = nil
	runner.responses["git -C "+source+" symbolic-ref --short refs/remotes/origin/HEAD"] = []byte("origin/main\n")
	runner.responses["git -C "+source+" rev-parse --verify origin/main^{commit}"] = []byte(strings.Repeat("a", 40) + "\n")
	catalog, err := NewWithRunner(filepath.Join(root, "state"), filepath.Join(root, "sources"), runner, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Enroll(context.Background(), "frostyard/updex"); err != nil {
		t.Fatal(err)
	}
	record, err := catalog.Setup(context.Background(), "frostyard/updex")
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusReady || record.BaseRef != "origin/main" || record.BaseCommit != strings.Repeat("a", 40) || record.PreparedAt == nil {
		t.Fatalf("unexpected prepared record: %#v", record)
	}
	wantFetch := "git -C " + source + " fetch --prune origin"
	if !contains(runner.calls, wantFetch) {
		t.Fatalf("calls %q do not contain %q", runner.calls, wantFetch)
	}
}

func TestSetupFallsBackToGitHubDefaultBranch(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources", "frostyard", "firn")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{responses: map[string][]byte{}, errors: map[string]error{}}
	runner.responses["git -C "+source+" remote get-url origin"] = []byte("git@github.com:frostyard/firn.git\n")
	runner.errors["git -C "+source+" symbolic-ref --short refs/remotes/origin/HEAD"] = errors.New("missing")
	runner.responses["gh repo view frostyard/firn --json defaultBranchRef --jq .defaultBranchRef.name"] = []byte("main\n")
	runner.responses["git -C "+source+" rev-parse --verify origin/main^{commit}"] = []byte(strings.Repeat("b", 64) + "\n")
	catalog, err := NewWithRunner(filepath.Join(root, "state"), filepath.Join(root, "sources"), runner, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Enroll(context.Background(), "frostyard/firn"); err != nil {
		t.Fatal(err)
	}
	record, err := catalog.Setup(context.Background(), "frostyard/firn")
	if err != nil {
		t.Fatal(err)
	}
	if record.BaseRef != "origin/main" || len(record.BaseCommit) != 64 {
		t.Fatalf("unexpected fallback result: %#v", record)
	}
}

func TestSetupFailsClosedOnOriginMismatchAndDirtyCheckout(t *testing.T) {
	for _, test := range []struct {
		name   string
		origin string
		status string
	}{
		{name: "origin", origin: "https://github.com/frostyard/other.git\n"},
		{name: "dirty", origin: "https://github.com/frostyard/updex.git\n", status: " M README.md\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			source := filepath.Join(root, "sources", "frostyard", "updex")
			if err := os.MkdirAll(source, 0o700); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{responses: map[string][]byte{}, errors: map[string]error{}}
			runner.responses["git -C "+source+" remote get-url origin"] = []byte(test.origin)
			runner.responses["git -C "+source+" status --porcelain=v1"] = []byte(test.status)
			catalog, err := NewWithRunner(filepath.Join(root, "state"), filepath.Join(root, "sources"), runner, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := catalog.Enroll(context.Background(), "frostyard/updex"); err != nil {
				t.Fatal(err)
			}
			record, err := catalog.Setup(context.Background(), "frostyard/updex")
			if !errors.Is(err, ErrConflict) || record.Status != StatusFailed {
				t.Fatalf("Setup() = %#v, %v; want conflict failure", record, err)
			}
		})
	}
}

func TestSetupCloneFailureDoesNotPersistCommandOutput(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "sources", "frostyard", "updex")
	call := "gh repo clone frostyard/updex " + source + " -- --origin origin"
	runner := &fakeRunner{responses: map[string][]byte{}, errors: map[string]error{call: errors.New("secret stderr")}}
	catalog, err := NewWithRunner(filepath.Join(root, "state"), filepath.Join(root, "sources"), runner, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Enroll(context.Background(), "frostyard/updex"); err != nil {
		t.Fatal(err)
	}
	record, err := catalog.Setup(context.Background(), "frostyard/updex")
	if err == nil || strings.Contains(record.Detail, "secret") {
		t.Fatalf("Setup() = %#v, %v; command detail leaked", record, err)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
