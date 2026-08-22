package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeRepositories struct {
	records []managedrepo.Record
}

func (repositories *fakeRepositories) List(context.Context) ([]managedrepo.Record, error) {
	return append([]managedrepo.Record(nil), repositories.records...), nil
}

func (repositories *fakeRepositories) Setup(_ context.Context, repository string) (managedrepo.Record, error) {
	for _, record := range repositories.records {
		if record.Repository == repository {
			now := time.Date(2026, 8, 21, 23, 0, 0, 0, time.UTC)
			record.Status = managedrepo.StatusReady
			record.BaseCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			record.PreparedAt = &now
			return record, nil
		}
	}
	return managedrepo.Record{}, managedrepo.ErrNotFound
}

type fakePreflights struct {
	mu    sync.Mutex
	calls []string
	err   error
}

func (preflights *fakePreflights) Refresh(_ context.Context, provider, server, repository string) (PreflightResult, error) {
	preflights.mu.Lock()
	defer preflights.mu.Unlock()
	preflights.calls = append(preflights.calls, provider+"/"+server+"/"+repository)
	return PreflightResult{Status: "ready", Detail: "live MCP proof passed", ExpiresAt: time.Now().Add(15 * time.Minute)}, preflights.err
}

type fakeQueue struct {
	counts map[string]map[queueview.Role]int
}

func (queue *fakeQueue) Observe(_ context.Context, repository string) (queueview.Snapshot, error) {
	counts := map[queueview.Role]int{
		queueview.RoleDiscoverer:  queue.counts[repository][queueview.RoleDiscoverer],
		queueview.RoleImplementer: queue.counts[repository][queueview.RoleImplementer],
		queueview.RoleReviewer:    queue.counts[repository][queueview.RoleReviewer],
	}
	return queueview.Snapshot{Repository: repository, Counts: counts, ObservedAt: time.Now().UTC()}, nil
}

type fakeWorkers struct {
	mu       sync.Mutex
	launched []worker.LaunchRequest
}

func (workers *fakeWorkers) List(context.Context) ([]worker.Record, error) {
	return []worker.Record{}, nil
}

func (workers *fakeWorkers) Launch(_ context.Context, request worker.LaunchRequest) (worker.Record, error) {
	workers.mu.Lock()
	defer workers.mu.Unlock()
	workers.launched = append(workers.launched, request)
	return worker.Record{ID: "worker-000000000000000" + string(rune('0'+len(workers.launched)))}, nil
}

func TestCampaignLaunchesAcrossEveryEnrolledRepositoryAndLane(t *testing.T) {
	repositories := &fakeRepositories{records: []managedrepo.Record{
		{Repository: "frostyard/firn", Source: "/sources/firn"},
		{Repository: "frostyard/updex", Source: "/sources/updex"},
	}}
	preflights := &fakePreflights{}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		"frostyard/firn":  {queueview.RoleDiscoverer: 1, queueview.RoleImplementer: 1},
		"frostyard/updex": {queueview.RoleDiscoverer: 1, queueview.RoleReviewer: 1},
	}}
	workers := &fakeWorkers{}
	controller := newTestController(t, repositories, preflights, queue, workers)
	request := validRequest()
	request.Discoverer.Capacity = 2
	request.Implementer.Capacity = 1
	request.Reviewer.Capacity = 1
	if _, err := controller.Start(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		workers.mu.Lock()
		defer workers.mu.Unlock()
		return len(workers.launched) == 4
	})
	if _, err := controller.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	controller.Close()

	workers.mu.Lock()
	defer workers.mu.Unlock()
	seen := make(map[string]bool)
	for _, launch := range workers.launched {
		seen[launch.Repository+"/"+launch.Role] = true
		if launch.Adapter != worker.AdapterOCI || launch.Runtime != worker.RuntimeDocker || launch.BaseRef != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Fatalf("campaign changed launch boundary: %#v", launch)
		}
	}
	for _, wanted := range []string{
		"frostyard/firn/discoverer", "frostyard/updex/discoverer",
		"frostyard/firn/implementer", "frostyard/updex/reviewer",
	} {
		if !seen[wanted] {
			t.Fatalf("missing launch %s in %#v", wanted, workers.launched)
		}
	}
	preflights.mu.Lock()
	defer preflights.mu.Unlock()
	if len(preflights.calls) != 2 {
		t.Fatalf("preflight calls = %q, want one per distinct provider", preflights.calls)
	}
}

func TestCampaignRequiresEnrollmentAndBoundedCapacity(t *testing.T) {
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, &fakeQueue{counts: map[string]map[queueview.Role]int{}}, &fakeWorkers{})
	if _, err := controller.Start(context.Background(), validRequest()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Start() error = %v, want invalid enrollment", err)
	}
	request := validRequest()
	request.Discoverer.Capacity = 6
	request.Implementer.Capacity = 6
	request.Reviewer.Capacity = 1
	if _, err := controller.Start(context.Background(), request); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Start() error = %v, want invalid total capacity", err)
	}
}

func TestSharedFailedProviderPreflightRunsOnceAtStart(t *testing.T) {
	repositories := &fakeRepositories{records: []managedrepo.Record{{Repository: "frostyard/firn", Source: "/sources/firn"}}}
	preflights := &fakePreflights{err: errors.New("failed proof")}
	controller := newTestController(t, repositories, preflights, &fakeQueue{counts: map[string]map[queueview.Role]int{"frostyard/firn": {}}}, &fakeWorkers{})
	if _, err := controller.Start(context.Background(), validRequest()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		preflights.mu.Lock()
		defer preflights.mu.Unlock()
		return len(preflights.calls) == 2
	})
	controller.Close()
	preflights.mu.Lock()
	defer preflights.mu.Unlock()
	if len(preflights.calls) != 2 {
		t.Fatalf("preflight calls = %q, want one Claude and one Copilot pair", preflights.calls)
	}
}

func TestExpiredPreflightRefreshesOnlyWhenWorkIsEligible(t *testing.T) {
	preflights := &fakePreflights{}
	controller := newTestController(t, &fakeRepositories{}, preflights, &fakeQueue{}, &fakeWorkers{})
	lane := Lane{Provider: "claude", MCPServer: "snowcat", Capacity: 1}
	repositories := []managedrepo.Record{{Repository: "frostyard/firn"}}
	expiries := map[string]time.Time{"claude\x00snowcat": time.Now().Add(-time.Minute)}
	if controller.ensureLanePreflight(context.Background(), lane, repositories, expiries, false) {
		t.Fatal("expired idle lane unexpectedly remained ready")
	}
	preflights.mu.Lock()
	if len(preflights.calls) != 0 {
		t.Fatalf("idle lane made preflight calls: %q", preflights.calls)
	}
	preflights.mu.Unlock()
	if !controller.ensureLanePreflight(context.Background(), lane, repositories, expiries, true) {
		t.Fatal("eligible lane did not refresh proof")
	}
	preflights.mu.Lock()
	defer preflights.mu.Unlock()
	if len(preflights.calls) != 1 {
		t.Fatalf("eligible lane preflight calls = %q, want one", preflights.calls)
	}
}

func TestNewMarksAnActiveCampaignInterrupted(t *testing.T) {
	stateDirectory := t.TempDir()
	content, err := json.Marshal(Record{ID: "campaign-old", Status: StatusRunning, Detail: "running"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDirectory, "campaign.json"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	controller, err := New(Config{
		StateDirectory: stateDirectory,
		Repositories:   &fakeRepositories{}, Preflights: &fakePreflights{},
		Queue: &fakeQueue{counts: map[string]map[queueview.Role]int{}}, Workers: &fakeWorkers{},
	})
	if err != nil {
		t.Fatal(err)
	}
	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusStopped || record.StoppedAt == nil {
		t.Fatalf("interrupted record = %#v", record)
	}
}

func newTestController(t *testing.T, repositories RepositoryCatalog, preflights Preflighter, queue queueview.Observer, workers WorkerManager) *Controller {
	t.Helper()
	controller, err := New(Config{
		StateDirectory: t.TempDir(), Repositories: repositories, Preflights: preflights,
		Queue: queue, Workers: workers,
		Random: func(bytes []byte) (int, error) {
			for index := range bytes {
				bytes[index] = byte(index + 1)
			}
			return len(bytes), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

func validRequest() Request {
	return Request{
		Adapter: worker.AdapterOCI, Runtime: worker.RuntimeDocker, IntervalSeconds: 10,
		Discoverer:  Lane{Provider: "claude", MCPServer: "snowcat", Capacity: 1},
		Implementer: Lane{Provider: "claude", MCPServer: "snowcat", Capacity: 1},
		Reviewer:    Lane{Provider: "copilot", MCPServer: "snowcat-mcp", Capacity: 1},
	}
}

func waitFor(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
