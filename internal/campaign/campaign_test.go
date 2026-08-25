package campaign

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeRepositories struct {
	mu           sync.Mutex
	records      []managedrepo.Record
	setupCalls   []string
	setupCommits map[string][]string
	setupErrors  map[string]error
}

func (repositories *fakeRepositories) List(context.Context) ([]managedrepo.Record, error) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	return append([]managedrepo.Record(nil), repositories.records...), nil
}

func (repositories *fakeRepositories) Setup(_ context.Context, repository string) (managedrepo.Record, error) {
	repositories.mu.Lock()
	defer repositories.mu.Unlock()
	repositories.setupCalls = append(repositories.setupCalls, repository)
	for index, record := range repositories.records {
		if record.Repository == repository {
			if err := repositories.setupErrors[repository]; err != nil {
				return record, err
			}
			now := time.Date(2026, 8, 21, 23, 0, 0, 0, time.UTC)
			record.Status = managedrepo.StatusReady
			record.BaseCommit = strings.Repeat("a", 40)
			if commits := repositories.setupCommits[repository]; len(commits) != 0 {
				record.BaseCommit = commits[0]
				repositories.setupCommits[repository] = commits[1:]
			}
			record.PreparedAt = &now
			repositories.records[index] = record
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
	mu           sync.Mutex
	counts       map[string]map[queueview.Role]int
	attempts     map[string]queueview.WorkerObservation
	attemptErr   map[string]error
	attemptCalls []string
}

func (queue *fakeQueue) Observe(_ context.Context, repository string) (queueview.Snapshot, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	counts := map[queueview.Role]int{
		queueview.RoleDiscoverer:  queue.counts[repository][queueview.RoleDiscoverer],
		queueview.RoleImplementer: queue.counts[repository][queueview.RoleImplementer],
		queueview.RoleReviewer:    queue.counts[repository][queueview.RoleReviewer],
	}
	return queueview.Snapshot{Repository: repository, Counts: counts, ObservedAt: time.Now().UTC()}, nil
}

func (queue *fakeQueue) ObserveWorker(_ context.Context, repository, workerID string) (queueview.WorkerObservation, error) {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	queue.attemptCalls = append(queue.attemptCalls, repository+"/"+workerID)
	if err := queue.attemptErr[workerID]; err != nil {
		return queueview.WorkerObservation{}, err
	}
	if observed, exists := queue.attempts[workerID]; exists {
		return observed, nil
	}
	return queueview.WorkerObservation{
		WorkerID: workerID, Repository: repository, Status: "unmatched",
		Detail: "no Snowcat attempt matched this worker",
	}, nil
}

type fakeWorkers struct {
	mu           sync.Mutex
	launched     []worker.LaunchRequest
	records      []worker.Record
	launchStatus string
}

func (workers *fakeWorkers) List(context.Context) ([]worker.Record, error) {
	workers.mu.Lock()
	defer workers.mu.Unlock()
	return append([]worker.Record(nil), workers.records...), nil
}

func (workers *fakeWorkers) Launch(_ context.Context, request worker.LaunchRequest) (worker.Record, error) {
	workers.mu.Lock()
	defer workers.mu.Unlock()
	workers.launched = append(workers.launched, request)
	record := worker.Record{
		ID:         "worker-000000000000000" + string(rune('0'+len(workers.launched))),
		Repository: request.Repository,
		Role:       request.Role,
		Status:     workers.launchStatus,
	}
	if workers.launchStatus != "" {
		workers.records = append(workers.records, record)
	}
	return record, nil
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

func TestCampaignStopSurfacesPersistenceFailureWhileStillCancellingAndRetaining(t *testing.T) {
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, &fakeQueue{counts: map[string]map[queueview.Role]int{}}, &fakeWorkers{})
	controller.record.Status = StatusRunning
	controller.statePath = filepath.Join(t.TempDir(), "missing-directory", "campaign.json")
	cancelled := false
	controller.cancel = func() { cancelled = true }

	_, err := controller.Stop(context.Background())
	if err == nil {
		t.Fatal("Stop error = nil, want board-campaign stop persistence failure surfaced")
	}
	if !cancelled {
		t.Fatal("Stop did not cancel future reconciliation despite the persistence failure")
	}

	record, getErr := controller.Get(context.Background())
	if getErr != nil {
		t.Fatal(getErr)
	}
	if record.Status != StatusStopping {
		t.Fatalf("status after failed stop persistence = %q, want %q (workers/workspaces stay retained, not force-stopped)", record.Status, StatusStopping)
	}
}

func TestCampaignRepinsManagedBaseImmediatelyBeforeEveryImplementerLaunch(t *testing.T) {
	repository := managedrepo.Record{
		Repository: "frostyard/firn", Source: "/sources/firn",
		BaseCommit: strings.Repeat("a", 40), Status: managedrepo.StatusReady,
	}
	freshCommits := []string{strings.Repeat("b", 40), strings.Repeat("c", 40)}
	repositories := &fakeRepositories{
		records:      []managedrepo.Record{repository},
		setupCommits: map[string][]string{repository.Repository: append([]string(nil), freshCommits...)},
	}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		repository.Repository: {queueview.RoleImplementer: 2},
	}}
	workers := &fakeWorkers{}
	controller := newTestController(t, repositories, &fakePreflights{}, queue, workers)
	controller.record.Status = StatusRunning
	controller.record.Request = validRequest()
	controller.record.Request.Implementer.Capacity = 2
	controller.record.Repositories = []RepositoryStatus{{
		Repository: repository.Repository, Source: repository.Source, BaseCommit: repository.BaseCommit, Status: StatusRunning,
	}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	repositories.mu.Lock()
	if len(repositories.setupCalls) != 2 {
		repositories.mu.Unlock()
		t.Fatalf("base refresh calls = %q, want one per implementer launch", repositories.setupCalls)
	}
	repositories.mu.Unlock()
	workers.mu.Lock()
	if len(workers.launched) != 2 {
		workers.mu.Unlock()
		t.Fatalf("launches = %d, want two", len(workers.launched))
	}
	for index, launch := range workers.launched {
		if launch.BaseRef != freshCommits[index] {
			workers.mu.Unlock()
			t.Fatalf("launch %d base = %q, want refreshed %q", index, launch.BaseRef, freshCommits[index])
		}
	}
	workers.mu.Unlock()
	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Repositories[0].BaseCommit != freshCommits[1] {
		t.Fatalf("campaign base = %q, want newest %q", record.Repositories[0].BaseCommit, freshCommits[1])
	}
}

func TestCampaignRefusesImplementerLaunchWhenBaseRefreshFails(t *testing.T) {
	repository := managedrepo.Record{
		Repository: "frostyard/firn", Source: "/sources/firn",
		BaseCommit: strings.Repeat("a", 40), Status: managedrepo.StatusReady,
	}
	repositories := &fakeRepositories{
		records:     []managedrepo.Record{repository},
		setupErrors: map[string]error{repository.Repository: errors.New("refresh unavailable")},
	}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		repository.Repository: {queueview.RoleImplementer: 1},
	}}
	workers := &fakeWorkers{}
	controller := newTestController(t, repositories, &fakePreflights{}, queue, workers)
	controller.record.Status = StatusRunning
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository, Status: StatusRunning}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 0 {
		t.Fatalf("launches = %d, want none after failed base refresh", len(workers.launched))
	}
	if !controller.inBackoff(repository.Repository, queueview.RoleImplementer) {
		t.Fatal("failed base refresh did not back off the implementer lane")
	}
	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusDegraded || record.Repositories[0].Detail != "implementer base refresh failed; retry is backed off" {
		t.Fatalf("campaign = %#v, want degraded base-refresh blocker", record)
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

func TestCampaignReportsEligibleProviderFailureAsDegraded(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		repository.Repository: {queueview.RoleDiscoverer: 1},
	}}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{err: errors.New("failed proof")}, queue, &fakeWorkers{})
	controller.record.Status = StatusRunning
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, map[string]time.Time{})
	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusDegraded || record.Providers[0].Status != StatusDegraded {
		t.Fatalf("campaign = %#v, want degraded provider blocker", record)
	}
	if record.Detail != "reconciliation blocked by 1 provider; ready lanes continue" {
		t.Fatalf("detail = %q", record.Detail)
	}
	if len(record.WorkerIDs) != 0 {
		t.Fatalf("workers launched through failed preflight: %q", record.WorkerIDs)
	}
}

func TestCampaignKeepsIdleRefreshNeededProvidersRunning(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{repository.Repository: {}}}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, &fakeWorkers{})
	controller.record.Status = StatusRunning
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, map[string]time.Time{})
	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusRunning {
		t.Fatalf("status = %q, want running idle campaign", record.Status)
	}
	for _, provider := range record.Providers {
		if provider.Status != "refresh-needed" {
			t.Fatalf("provider = %#v, want refresh-needed", provider)
		}
	}
}

func TestCampaignBacksOffWorkerThatExitsBeforeLaunchStabilizes(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		repository.Repository: {queueview.RoleReviewer: 1},
	}}
	workers := &fakeWorkers{launchStatus: worker.StatusExited}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, workers)
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 1 {
		t.Fatalf("launches = %d, want one before startup backoff", len(workers.launched))
	}
	if !controller.inBackoff(repository.Repository, queueview.RoleReviewer) {
		t.Fatal("reviewer startup exit did not enter launch backoff")
	}
}

func TestCampaignDoesNotBackOffWorkerAfterLaunchStabilizes(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		repository.Repository: {queueview.RoleReviewer: 1},
	}}
	workers := &fakeWorkers{launchStatus: worker.StatusRunning}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, workers)
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	workers.mu.Lock()
	workers.records[0].Status = worker.StatusExited
	workers.mu.Unlock()
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 2 {
		t.Fatalf("launches = %d, want stable worker exit to permit refill", len(workers.launched))
	}
	if controller.inBackoff(repository.Repository, queueview.RoleReviewer) {
		t.Fatal("stable worker exit unexpectedly entered launch backoff")
	}
}

func TestCampaignDoesNotRelaunchIntoTheSameStillQueuedItem(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/updex", Source: "/sources/updex", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	workerID := "worker-0000000000000001"
	queue := &fakeQueue{counts: map[string]map[queueview.Role]int{
		// Snowcat keeps reporting this one queued item on every observation
		// until worker-0000000000000001 actually calls claim_work.
		repository.Repository: {queueview.RoleReviewer: 1},
	}}
	workers := &fakeWorkers{launchStatus: worker.StatusRunning}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, workers)
	controller.record.Request = validRequest()
	controller.record.Request.Reviewer.Capacity = 2
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	// Tick 1 launches the only worker the single queued item justifies.
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	// Ticks 2 and 3: the launched worker is running but has not yet claimed
	// anything (the launch-to-claim gap, frostyard/snowcat-cockpit#17), so
	// Snowcat still reports the same one item as claimable. Spare capacity
	// (2) MUST NOT relaunch into it a second time.
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	if len(workers.launched) != 1 {
		workers.mu.Unlock()
		t.Fatalf("launches = %d, want exactly one worker for the one still-queued item", len(workers.launched))
	}
	workers.mu.Unlock()

	// Once the worker's attempt is confirmed, spare capacity is free again
	// for genuinely new work.
	queue.mu.Lock()
	queue.attempts = map[string]queueview.WorkerObservation{
		workerID: {WorkerID: workerID, Repository: repository.Repository, Status: "claimed", Detail: "Snowcat reports an active lease for this worker"},
	}
	queue.counts[repository.Repository] = map[queueview.Role]int{queueview.RoleReviewer: 1}
	queue.mu.Unlock()
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 2 {
		t.Fatalf("launches = %d, want a second launch once the first worker's claim is confirmed and a new item remains", len(workers.launched))
	}
}

func TestCampaignFailsStableLaneWhenProviderExitsWithClaimedAttempt(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	workerID := "worker-0000000000000001"
	queue := &fakeQueue{
		counts: map[string]map[queueview.Role]int{
			repository.Repository: {queueview.RoleReviewer: 1},
		},
		attempts: map[string]queueview.WorkerObservation{
			workerID: {WorkerID: workerID, Repository: repository.Repository, Status: "claimed", Detail: "Snowcat reports an active lease for this worker"},
		},
	}
	workers := &fakeWorkers{launchStatus: worker.StatusRunning}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, workers)
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	workers.mu.Lock()
	workers.records[0].Status = worker.StatusExited
	workers.mu.Unlock()
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusDegraded || len(record.Repositories) != 1 || record.Repositories[0].Status != StatusDegraded {
		t.Fatalf("campaign = %#v, want degraded lane failure", record)
	}
	wantDetail := "reviewer lane failed: provider exited without a terminal Snowcat outcome; retry is backed off"
	if record.Repositories[0].Detail != wantDetail {
		t.Fatalf("detail = %q, want %q", record.Repositories[0].Detail, wantDetail)
	}
	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 1 {
		t.Fatalf("launches = %d, want no refill after claimed exit", len(workers.launched))
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if len(queue.attemptCalls) != 2 {
		t.Fatalf("attempt observations = %q, want one pending-claim confirmation plus one exit outcome", queue.attemptCalls)
	}
}

func TestCampaignPausesLaneUntilExitedWorkerOutcomeCanBeObserved(t *testing.T) {
	repository := managedrepo.Record{Repository: "frostyard/firn", Source: "/sources/firn", BaseCommit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	workerID := "worker-0000000000000001"
	queue := &fakeQueue{
		counts: map[string]map[queueview.Role]int{
			repository.Repository: {queueview.RoleReviewer: 1},
		},
		attemptErr: map[string]error{workerID: errors.New("observation unavailable")},
	}
	workers := &fakeWorkers{launchStatus: worker.StatusRunning}
	controller := newTestController(t, &fakeRepositories{}, &fakePreflights{}, queue, workers)
	controller.record.Request = validRequest()
	controller.record.Repositories = []RepositoryStatus{{Repository: repository.Repository}}
	expiries := map[string]time.Time{
		"claude\x00snowcat":      time.Now().Add(15 * time.Minute),
		"copilot\x00snowcat-mcp": time.Now().Add(15 * time.Minute),
	}

	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)
	workers.mu.Lock()
	workers.records[0].Status = worker.StatusExited
	workers.mu.Unlock()
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	record, err := controller.Get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if record.Status != StatusDegraded || record.Repositories[0].Detail != "reviewer worker exit outcome observation failed; lane refill is paused" {
		t.Fatalf("campaign = %#v, want paused exit reconciliation", record)
	}
	workers.mu.Lock()
	if len(workers.launched) != 1 {
		workers.mu.Unlock()
		t.Fatalf("launches = %d, want lane held", len(workers.launched))
	}
	workers.mu.Unlock()

	queue.mu.Lock()
	delete(queue.attemptErr, workerID)
	queue.attempts = map[string]queueview.WorkerObservation{
		workerID: {WorkerID: workerID, Repository: repository.Repository, Status: "completed", Detail: "Snowcat reports this worker attempt as completed"},
	}
	queue.mu.Unlock()
	controller.reconcile(context.Background(), []managedrepo.Record{repository}, expiries)

	workers.mu.Lock()
	defer workers.mu.Unlock()
	if len(workers.launched) != 2 {
		t.Fatalf("launches = %d, want refill after terminal outcome", len(workers.launched))
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

func newTestController(t *testing.T, repositories RepositoryCatalog, preflights Preflighter, queue QueueObserver, workers WorkerManager) *Controller {
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
