package campaign

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

const (
	StatusStarting         = "starting"
	StatusRunning          = "running"
	StatusDegraded         = "degraded"
	StatusStopping         = "stopping"
	StatusStopped          = "stopped"
	StatusNeverRun         = "never-run"
	maxTotalCapacity       = 12
	defaultInterval        = 30 * time.Second
	minimumInterval        = 10 * time.Second
	maximumInterval        = 5 * time.Minute
	preflightRefreshWindow = 2 * time.Minute

	// haltedDetail and stoppedAfterStateFailureDetail are the operator-facing
	// reasons reported when the campaign cannot persist its own durable state.
	// They name the failure without echoing the underlying filesystem error, so
	// no path or other host detail reaches the node API.
	haltedDetail                   = "board campaign state persistence failed; reconciliation is halted and no further workers launch"
	stoppedAfterStateFailureDetail = "board campaign stopped after a state persistence failure; stored campaign state is stale and managed workers and workspaces remain retained"
)

var campaignNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

var (
	ErrInvalid     = errors.New("invalid board campaign")
	ErrConflict    = errors.New("board campaign conflict")
	ErrUnavailable = errors.New("board campaign unavailable")
)

type Lane struct {
	Provider  string `json:"provider"`
	MCPServer string `json:"mcpServer"`
	Capacity  int    `json:"capacity"`
}

type Request struct {
	Adapter         string `json:"adapter"`
	Runtime         string `json:"runtime,omitempty"`
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	Discoverer      Lane   `json:"discoverer"`
	Implementer     Lane   `json:"implementer"`
	Reviewer        Lane   `json:"reviewer"`
}

type RepositoryStatus struct {
	Repository string    `json:"repository"`
	Source     string    `json:"source"`
	BaseCommit string    `json:"baseCommit,omitempty"`
	Status     string    `json:"status"`
	Detail     string    `json:"detail"`
	ObservedAt time.Time `json:"observedAt,omitempty"`
}

type ProviderStatus struct {
	Provider  string    `json:"provider"`
	MCPServer string    `json:"mcpServer"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

type launchProbe struct {
	repository string
	role       queueview.Role
	stabilized bool
	claimed    bool
}

var launchableRoles = []queueview.Role{queueview.RoleDiscoverer, queueview.RoleImplementer, queueview.RoleReviewer}

type workerExit struct {
	workerID string
	status   string
	probe    launchProbe
}

type laneFailure struct {
	repository string
	role       queueview.Role
	detail     string
	retryAt    time.Time
}

type Record struct {
	ID           string             `json:"id,omitempty"`
	Status       string             `json:"status"`
	Detail       string             `json:"detail"`
	Request      Request            `json:"request"`
	Repositories []RepositoryStatus `json:"repositories"`
	Providers    []ProviderStatus   `json:"providers"`
	WorkerIDs    []string           `json:"workerIds"`
	StartedAt    time.Time          `json:"startedAt,omitempty"`
	UpdatedAt    time.Time          `json:"updatedAt,omitempty"`
	StoppedAt    *time.Time         `json:"stoppedAt,omitempty"`

	// WorkspacesCleaned and LastCleanupAt report automatic cleanup of
	// terminal worker workspaces performed by this campaign run.
	WorkspacesCleaned int       `json:"workspacesCleaned,omitempty"`
	LastCleanupAt     time.Time `json:"lastCleanupAt,omitempty"`
}

type PreflightResult struct {
	Status    string
	Detail    string
	ExpiresAt time.Time
}

type RepositoryCatalog interface {
	List(context.Context) ([]managedrepo.Record, error)
	Setup(context.Context, string) (managedrepo.Record, error)
}

type Preflighter interface {
	Refresh(context.Context, string, string, string) (PreflightResult, error)
}

type WorkerManager interface {
	List(context.Context) ([]worker.Record, error)
	Launch(context.Context, worker.LaunchRequest) (worker.Record, error)
	Cleanup(context.Context, string, worker.CleanupOptions) (worker.Record, error)
}

// RetentionPolicy bounds how many terminal, nothing-left-to-matter worker
// workspaces the campaign controller keeps before cleaning them
// automatically. Count and Age are mutually exclusive. Configured must be
// true for the policy to take effect at all: an operator or test that never
// sets this field gets the zero value, Configured false, and sees no
// behavior change, while an explicit Count of 0 (Configured true) cleans
// every eligible candidate on each sweep.
type RetentionPolicy struct {
	Configured bool
	Count      int
	Age        time.Duration
}

type QueueObserver interface {
	queueview.Observer
	queueview.AttemptObserver
}

type Config struct {
	StateDirectory string
	Repositories   RepositoryCatalog
	Preflights     Preflighter
	Queue          QueueObserver
	Workers        WorkerManager
	Now            func() time.Time
	Random         func([]byte) (int, error)

	// RetainWorkspaces bounds automatic cleanup of terminal worker
	// workspaces. See RetentionPolicy.
	RetainWorkspaces RetentionPolicy
}

type Controller struct {
	statePath    string
	repositories RepositoryCatalog
	preflights   Preflighter
	queue        QueueObserver
	workers      WorkerManager
	now          func() time.Time
	random       func([]byte) (int, error)

	retain RetentionPolicy

	mu                sync.Mutex
	record            Record
	stateFailed       bool
	cancel            context.CancelFunc
	done              chan struct{}
	backoff           map[string]time.Time
	probes            map[string]launchProbe
	laneFailures      map[string]laneFailure
	cleanupCandidates map[string]time.Time
}

func New(config Config) (*Controller, error) {
	if config.StateDirectory == "" || config.Repositories == nil || config.Preflights == nil || config.Queue == nil || config.Workers == nil {
		return nil, fmt.Errorf("%w: state, repositories, preflight, queue, and workers are required", ErrUnavailable)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Random == nil {
		config.Random = rand.Read
	}
	if err := os.MkdirAll(config.StateDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("open board campaign state: %w", err)
	}
	controller := &Controller{
		statePath:         filepath.Join(config.StateDirectory, "campaign.json"),
		repositories:      config.Repositories,
		preflights:        config.Preflights,
		queue:             config.Queue,
		workers:           config.Workers,
		now:               config.Now,
		random:            config.Random,
		retain:            config.RetainWorkspaces,
		backoff:           make(map[string]time.Time),
		probes:            make(map[string]launchProbe),
		laneFailures:      make(map[string]laneFailure),
		cleanupCandidates: make(map[string]time.Time),
	}
	record, err := controller.read()
	if err != nil {
		return nil, err
	}
	if record.Status == "" {
		record = Record{Status: StatusNeverRun, Detail: "no board campaign has run on this node", Repositories: []RepositoryStatus{}, Providers: []ProviderStatus{}, WorkerIDs: []string{}}
	}
	if isActive(record.Status) {
		now := controller.now().UTC()
		record.Status = StatusStopped
		record.Detail = "previous board campaign was interrupted by node restart; no workers were stopped"
		record.UpdatedAt = now
		record.StoppedAt = &now
		if err := controller.write(record); err != nil {
			return nil, err
		}
	}
	controller.record = record
	return controller, nil
}

func (controller *Controller) Start(ctx context.Context, request Request) (Record, error) {
	if err := validateRequest(&request); err != nil {
		return Record{}, err
	}
	repositories, err := controller.repositories.List(ctx)
	if err != nil {
		return Record{}, fmt.Errorf("list managed repositories: %w", err)
	}
	if len(repositories) == 0 {
		return Record{}, fmt.Errorf("%w: enroll at least one managed repository", ErrInvalid)
	}

	controller.mu.Lock()
	defer controller.mu.Unlock()
	if isActive(controller.record.Status) {
		return Record{}, fmt.Errorf("%w: a board campaign is already active", ErrConflict)
	}
	id, err := controller.newID()
	if err != nil {
		return Record{}, fmt.Errorf("create board campaign ID: %w", err)
	}
	now := controller.now().UTC()
	repositoryStates := make([]RepositoryStatus, 0, len(repositories))
	for _, repository := range repositories {
		repositoryStates = append(repositoryStates, RepositoryStatus{Repository: repository.Repository, Source: repository.Source, Status: StatusStarting, Detail: "managed source setup queued"})
	}
	controller.record = Record{
		ID: id, Status: StatusStarting, Detail: "preparing all enrolled repositories and provider preflights",
		Request: request, Repositories: repositoryStates, Providers: []ProviderStatus{}, WorkerIDs: []string{}, StartedAt: now, UpdatedAt: now,
	}
	if err := controller.write(controller.record); err != nil {
		return Record{}, err
	}
	// The halt latch belongs to the campaign whose state stopped persisting,
	// not to the controller. This campaign's own initial state just persisted,
	// so it starts unhalted instead of inheriting an earlier campaign's failure
	// and refusing to launch any worker for the rest of the process lifetime.
	controller.stateFailed = false
	runContext, cancel := context.WithCancel(context.Background())
	controller.cancel = cancel
	controller.done = make(chan struct{})
	controller.backoff = make(map[string]time.Time)
	controller.probes = make(map[string]launchProbe)
	controller.laneFailures = make(map[string]laneFailure)
	controller.cleanupCandidates = make(map[string]time.Time)
	go controller.run(runContext)
	return cloneRecord(controller.record), nil
}

func (controller *Controller) Stop(_ context.Context) (Record, error) {
	controller.mu.Lock()
	if !isActive(controller.record.Status) {
		record := cloneRecord(controller.record)
		controller.mu.Unlock()
		return record, nil
	}
	controller.record.Status = StatusStopping
	controller.record.Detail = "stopping future campaign reconciliation; workers remain retained"
	controller.record.UpdatedAt = controller.now().UTC()
	writeErr := controller.write(controller.record)
	cancel := controller.cancel
	record := cloneRecord(controller.record)
	controller.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if writeErr != nil {
		return record, fmt.Errorf("persist board campaign stopping state: %w", writeErr)
	}
	return record, nil
}

func (controller *Controller) Get(_ context.Context) (Record, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return cloneRecord(controller.record), nil
}

func (controller *Controller) Close() {
	controller.mu.Lock()
	cancel := controller.cancel
	done := controller.done
	controller.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// finalizeStopped records the campaign's stopped state once reconciliation has
// ended and releases Close waiters. A failed write here cannot be retried away:
// the stored state is stale, so the in-memory record carries the reason instead
// and Get reports it to the operator. A campaign already halted by an earlier
// persistence failure keeps that reason through finalization even when this
// final write succeeds, because the writes it lost are still lost.
func (controller *Controller) finalizeStopped() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.now().UTC()
	controller.record.Status = StatusStopped
	controller.record.Detail = "board campaign stopped; managed workers and workspaces remain retained"
	if controller.stateFailed {
		controller.record.Detail = stoppedAfterStateFailureDetail
	}
	controller.record.UpdatedAt = now
	controller.record.StoppedAt = &now
	controller.persistLocked()
	controller.cancel = nil
	close(controller.done)
}

func (controller *Controller) run(ctx context.Context) {
	defer controller.finalizeStopped()

	readyRepositories := controller.setupRepositories(ctx)
	if ctx.Err() != nil {
		return
	}
	providerExpiries := controller.refreshPreflights(ctx, readyRepositories)
	if ctx.Err() != nil {
		return
	}
	controller.setRunningStatus(len(readyRepositories) > 0 && len(providerExpiries) > 0)
	interval := time.Duration(controller.request().IntervalSeconds) * time.Second
	if interval == 0 {
		interval = defaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		controller.reconcile(ctx, readyRepositories, providerExpiries)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			readyRepositories = controller.retryRepositorySetup(ctx, readyRepositories)
		}
	}
}

func (controller *Controller) setupRepositories(ctx context.Context) []managedrepo.Record {
	repositories, err := controller.repositories.List(ctx)
	if err != nil {
		controller.degrade("managed repository listing failed")
		return nil
	}
	type result struct {
		record managedrepo.Record
		err    error
	}
	results := make(chan result, len(repositories))
	semaphore := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, repository := range repositories {
		repository := repository
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			prepared, setupErr := controller.repositories.Setup(ctx, repository.Repository)
			results <- result{prepared, setupErr}
		}()
	}
	group.Wait()
	close(results)
	ready := make([]managedrepo.Record, 0, len(repositories))
	for result := range results {
		status := result.record.Status
		detail := result.record.Detail
		if result.err == nil && status == managedrepo.StatusReady {
			ready = append(ready, result.record)
		} else {
			controller.setNamedBackoff("setup\x00" + result.record.Repository)
			status = StatusDegraded
			if detail == "" {
				detail = "managed source setup failed"
			}
		}
		controller.updateRepository(result.record.Repository, func(state *RepositoryStatus) {
			state.Source = result.record.Source
			state.BaseCommit = result.record.BaseCommit
			state.Status = status
			state.Detail = detail
		})
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Repository < ready[j].Repository })
	return ready
}

func (controller *Controller) retryRepositorySetup(ctx context.Context, ready []managedrepo.Record) []managedrepo.Record {
	all, err := controller.repositories.List(ctx)
	if err != nil {
		controller.degrade("managed repository listing failed")
		return ready
	}
	readySet := make(map[string]bool, len(ready))
	for _, repository := range ready {
		readySet[repository.Repository] = true
	}
	for _, repository := range all {
		key := "setup\x00" + repository.Repository
		if readySet[repository.Repository] || controller.inNamedBackoff(key) {
			continue
		}
		prepared, setupErr := controller.repositories.Setup(ctx, repository.Repository)
		if setupErr != nil || prepared.Status != managedrepo.StatusReady {
			controller.setNamedBackoff(key)
			continue
		}
		ready = append(ready, prepared)
		readySet[prepared.Repository] = true
		controller.updateRepository(prepared.Repository, func(state *RepositoryStatus) {
			state.Source = prepared.Source
			state.BaseCommit = prepared.BaseCommit
			state.Status = StatusRunning
			state.Detail = prepared.Detail
		})
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Repository < ready[j].Repository })
	return ready
}

func (controller *Controller) refreshPreflights(ctx context.Context, repositories []managedrepo.Record) map[string]time.Time {
	result := make(map[string]time.Time)
	seen := make(map[string]bool)
	if len(repositories) == 0 {
		return result
	}
	for _, lane := range controller.lanes() {
		key := lane.Provider + "\x00" + lane.MCPServer
		if seen[key] {
			continue
		}
		seen[key] = true
		preflight, err := controller.preflights.Refresh(ctx, lane.Provider, lane.MCPServer, repositories[0].Repository)
		status := preflight.Status
		detail := preflight.Detail
		if err != nil {
			status = StatusDegraded
			controller.setNamedBackoff("preflight\x00" + key)
			if detail == "" {
				detail = "provider preflight failed after two attempts"
			}
		} else {
			result[key] = preflight.ExpiresAt
		}
		controller.updateProvider(ProviderStatus{Provider: lane.Provider, MCPServer: lane.MCPServer, Status: status, Detail: detail, ExpiresAt: preflight.ExpiresAt})
	}
	return result
}

func (controller *Controller) reconcile(ctx context.Context, repositories []managedrepo.Record, expiries map[string]time.Time) {
	if ctx.Err() != nil || controller.halted() {
		return
	}
	workers, err := controller.workers.List(ctx)
	if err != nil {
		controller.degrade("managed worker inventory failed")
		return
	}
	active := map[queueview.Role]int{}
	for _, record := range workers {
		if record.Status == worker.StatusRunning || record.Status == worker.StatusAllocating {
			active[queueview.Role(record.Role)]++
		}
	}
	exits := controller.inspectLaunchProbes(workers)
	controller.confirmPendingClaims(ctx, exits)

	type observation struct {
		repository managedrepo.Record
		snapshot   queueview.Snapshot
		err        error
	}
	observations := make(chan observation, len(repositories))
	semaphore := make(chan struct{}, 4)
	var group sync.WaitGroup
	for _, repository := range repositories {
		repository := repository
		group.Add(1)
		go func() {
			defer group.Done()
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				return
			}
			snapshot, observeErr := controller.queue.Observe(ctx, repository.Repository)
			observations <- observation{repository, snapshot, observeErr}
		}()
	}
	group.Wait()
	close(observations)
	byRepository := make(map[string]observation, len(repositories))
	for observed := range observations {
		byRepository[observed.repository.Repository] = observed
		if observed.err != nil {
			controller.updateRepository(observed.repository.Repository, func(state *RepositoryStatus) {
				state.Status = StatusDegraded
				state.Detail = "Snowcat queue observation failed; this repository will retry later"
			})
			continue
		}
		controller.updateRepository(observed.repository.Repository, func(state *RepositoryStatus) {
			state.Status = StatusRunning
			state.Detail = fmt.Sprintf("claimable: %d discoverer, %d implementer, %d reviewer", observed.snapshot.Counts[queueview.RoleDiscoverer], observed.snapshot.Counts[queueview.RoleImplementer], observed.snapshot.Counts[queueview.RoleReviewer])
			state.ObservedAt = observed.snapshot.ObservedAt
		})
	}
	pendingFailures := controller.reconcileWorkerExits(ctx, exits)
	controller.sweepCleanupCandidates(ctx)
	// Resolved after reconcileWorkerExits so a probe that this same tick
	// confirmed exited (claimed or otherwise) no longer reserves its slot.
	pendingClaims := controller.pendingLaunchCounts()
	for repository, observed := range byRepository {
		if observed.err != nil {
			continue
		}
		for _, role := range launchableRoles {
			reserved := pendingClaims[laneFailureKey(repository, role)]
			if reserved <= 0 {
				continue
			}
			if observed.snapshot.Counts[role] > reserved {
				observed.snapshot.Counts[role] -= reserved
			} else {
				observed.snapshot.Counts[role] = 0
			}
		}
	}
	laneFailures := append(controller.activeLaneFailures(), pendingFailures...)
	sort.Slice(laneFailures, func(i, j int) bool {
		if laneFailures[i].repository == laneFailures[j].repository {
			return laneFailures[i].role < laneFailures[j].role
		}
		return laneFailures[i].repository < laneFailures[j].repository
	})
	blockedRoles := make(map[queueview.Role]bool, len(laneFailures))
	for _, failure := range laneFailures {
		blockedRoles[failure.role] = true
		controller.updateRepository(failure.repository, func(state *RepositoryStatus) {
			state.Status = StatusDegraded
			state.Detail = failure.detail
		})
	}

	roles := launchableRoles
	eligibleByRole := make(map[queueview.Role]int, len(roles))
	providerEligible := make(map[string]bool)
	for _, role := range roles {
		if blockedRoles[role] {
			continue
		}
		lane := controller.lane(role)
		for _, observed := range byRepository {
			if observed.err == nil {
				eligibleByRole[role] += observed.snapshot.Counts[role]
			}
		}
		if eligibleByRole[role] > 0 {
			providerEligible[lane.Provider+"\x00"+lane.MCPServer] = true
		}
	}
	preflightReady := make(map[string]bool)
	for _, role := range roles {
		if blockedRoles[role] {
			continue
		}
		lane := controller.lane(role)
		key := lane.Provider + "\x00" + lane.MCPServer
		if _, checked := preflightReady[key]; checked {
			continue
		}
		preflightReady[key] = controller.ensureLanePreflight(ctx, lane, repositories, expiries, providerEligible[key])
	}

	for _, role := range roles {
		lane := controller.lane(role)
		if !preflightReady[lane.Provider+"\x00"+lane.MCPServer] {
			continue
		}
		remaining := lane.Capacity - active[role]
		for remaining > 0 {
			launchedThisPass := false
			for _, repository := range repositories {
				if remaining == 0 {
					break
				}
				if ctx.Err() != nil || controller.halted() {
					return
				}
				observed, ok := byRepository[repository.Repository]
				if !ok || observed.err != nil || observed.snapshot.Counts[role] <= 0 {
					continue
				}
				if controller.inBackoff(repository.Repository, role) {
					controller.updateRepository(repository.Repository, func(state *RepositoryStatus) {
						state.Status = StatusDegraded
						state.Detail = string(role) + " launch retry is backed off"
					})
					continue
				}
				launchRepository := repository
				if role == queueview.RoleImplementer {
					var refreshed bool
					launchRepository, refreshed = controller.refreshImplementationBase(ctx, repository)
					if !refreshed {
						continue
					}
				}
				record, launchErr := controller.workers.Launch(ctx, worker.LaunchRequest{
					Adapter: controller.request().Adapter, Runtime: controller.request().Runtime,
					Provider: lane.Provider, MCPServer: lane.MCPServer, Role: string(role), Repository: launchRepository.Repository,
					Source: launchRepository.Source, BaseRef: launchRepository.BaseCommit,
				})
				if launchErr != nil {
					controller.setBackoff(repository.Repository, role)
					controller.updateRepository(repository.Repository, func(state *RepositoryStatus) {
						state.Status = StatusDegraded
						state.Detail = string(role) + " launch failed; retry is backed off"
					})
					continue
				}
				controller.addWorker(record.ID)
				controller.addLaunchProbe(record.ID, repository.Repository, role)
				observed.snapshot.Counts[role]--
				byRepository[repository.Repository] = observed
				remaining--
				launchedThisPass = true
			}
			if !launchedThisPass {
				break
			}
		}
	}
	controller.setReconciledStatus()
}

func (controller *Controller) refreshImplementationBase(ctx context.Context, repository managedrepo.Record) (managedrepo.Record, bool) {
	refreshed, err := controller.repositories.Setup(ctx, repository.Repository)
	if err != nil || refreshed.Status != managedrepo.StatusReady || refreshed.BaseCommit == "" || refreshed.PreparedAt == nil || refreshed.Repository != repository.Repository || refreshed.Source != repository.Source {
		controller.setBackoff(repository.Repository, queueview.RoleImplementer)
		controller.updateRepository(repository.Repository, func(state *RepositoryStatus) {
			state.Status = StatusDegraded
			state.Detail = "implementer base refresh failed; retry is backed off"
		})
		return managedrepo.Record{}, false
	}
	controller.updateRepository(repository.Repository, func(state *RepositoryStatus) {
		state.Source = refreshed.Source
		state.BaseCommit = refreshed.BaseCommit
	})
	return refreshed, true
}

func (controller *Controller) addLaunchProbe(workerID, repository string, role queueview.Role) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.probes[workerID] = launchProbe{repository: repository, role: role}
}

func (controller *Controller) inspectLaunchProbes(workers []worker.Record) []workerExit {
	byID := make(map[string]worker.Record, len(workers))
	for _, record := range workers {
		byID[record.ID] = record
	}
	controller.mu.Lock()
	defer controller.mu.Unlock()
	exits := make([]workerExit, 0)
	for workerID, probe := range controller.probes {
		record, exists := byID[workerID]
		if !exists {
			continue
		}
		switch record.Status {
		case worker.StatusRunning:
			probe.stabilized = true
			controller.probes[workerID] = probe
		case worker.StatusExited, worker.StatusFailed:
			exits = append(exits, workerExit{workerID: workerID, status: record.Status, probe: probe})
		case worker.StatusStopped, worker.StatusCleaned:
			delete(controller.probes, workerID)
		}
	}
	sort.Slice(exits, func(i, j int) bool { return exits[i].workerID < exits[j].workerID })
	return exits
}

func (controller *Controller) reconcileWorkerExits(ctx context.Context, exits []workerExit) []laneFailure {
	pending := make([]laneFailure, 0)
	for _, exited := range exits {
		observed, err := controller.queue.ObserveWorker(ctx, exited.probe.repository, exited.workerID)
		if err != nil {
			pending = append(pending, laneFailure{
				repository: exited.probe.repository,
				role:       exited.probe.role,
				detail:     string(exited.probe.role) + " worker exit outcome observation failed; lane refill is paused",
			})
			continue
		}

		switch observed.Status {
		case "completed", "blocked", "released", "expired":
			controller.removeLaunchProbe(exited.workerID)
			if exited.status == worker.StatusExited {
				controller.markCleanupCandidate(exited.workerID)
			}
		case "unmatched":
			controller.removeLaunchProbe(exited.workerID)
			if !exited.probe.stabilized {
				controller.failLane(exited.probe, string(exited.probe.role)+" provider exited before launch stabilized; lane retry is backed off")
			} else if exited.status == worker.StatusFailed {
				controller.failLane(exited.probe, string(exited.probe.role)+" lane failed: retained worker terminal failed without a terminal Snowcat outcome; retry is backed off")
			} else {
				// The provider exited cleanly having stabilized but claimed
				// nothing (the lane found nothing to claim) — nothing about
				// this workspace can still matter.
				controller.markCleanupCandidate(exited.workerID)
			}
		case "claimed":
			controller.removeLaunchProbe(exited.workerID)
			controller.failLane(exited.probe, string(exited.probe.role)+" lane failed: provider exited without a terminal Snowcat outcome; retry is backed off")
		case "ambiguous":
			controller.removeLaunchProbe(exited.workerID)
			controller.failLane(exited.probe, string(exited.probe.role)+" lane failed: provider exit did not correlate to one Snowcat attempt; retry is backed off")
		default:
			pending = append(pending, laneFailure{
				repository: exited.probe.repository,
				role:       exited.probe.role,
				detail:     string(exited.probe.role) + " worker exit returned an unknown Snowcat outcome; lane refill is paused",
			})
		}
	}
	return pending
}

func (controller *Controller) removeLaunchProbe(workerID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	delete(controller.probes, workerID)
}

// markCleanupCandidate records that a worker's workspace has nothing left
// that can still matter (its provider process exited, and its item reached
// a terminal Snowcat outcome or its lane found nothing to claim). It does
// not clean the workspace itself; sweepCleanupCandidates enforces the
// configured retention bound and performs the actual cleanup.
func (controller *Controller) markCleanupCandidate(workerID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if _, tracked := controller.cleanupCandidates[workerID]; !tracked {
		controller.cleanupCandidates[workerID] = controller.now().UTC()
	}
}

type cleanupCandidate struct {
	workerID   string
	eligibleAt time.Time
}

// sweepCleanupCandidates cleans workspaces that became cleanup candidates
// and now exceed the configured retention bound (a count of the
// newest-eligible candidates to keep, or a maximum age). A worker whose
// Cleanup call fails (for example an unclean tree) is left retained and
// reconsidered on the next reconcile tick, matching today's manual-cleanup
// failure behavior.
func (controller *Controller) sweepCleanupCandidates(ctx context.Context) {
	policy := controller.retain
	if !policy.Configured {
		return
	}
	controller.mu.Lock()
	candidates := make([]cleanupCandidate, 0, len(controller.cleanupCandidates))
	for workerID, eligibleAt := range controller.cleanupCandidates {
		candidates = append(candidates, cleanupCandidate{workerID: workerID, eligibleAt: eligibleAt})
	}
	controller.mu.Unlock()
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].eligibleAt.Before(candidates[j].eligibleAt) })

	due := make([]string, 0, len(candidates))
	if policy.Age > 0 {
		cutoff := controller.now().UTC().Add(-policy.Age)
		for _, candidate := range candidates {
			if candidate.eligibleAt.Before(cutoff) {
				due = append(due, candidate.workerID)
			}
		}
	} else if len(candidates) > policy.Count {
		for _, candidate := range candidates[:len(candidates)-policy.Count] {
			due = append(due, candidate.workerID)
		}
	}

	for _, workerID := range due {
		if ctx.Err() != nil {
			return
		}
		if _, err := controller.workers.Cleanup(ctx, workerID, worker.CleanupOptions{}); err != nil {
			continue
		}
		controller.mu.Lock()
		delete(controller.cleanupCandidates, workerID)
		controller.mu.Unlock()
		controller.recordCleanup()
	}
}

func (controller *Controller) recordCleanup() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.now().UTC()
	controller.record.WorkspacesCleaned++
	controller.record.LastCleanupAt = now
	controller.record.UpdatedAt = now
	controller.persistLocked()
}

// confirmPendingClaims checks whether a stabilized (running) launch probe has
// made its first Snowcat attempt yet, without waiting for it to exit. Once
// confirmed, pendingLaunchCounts stops reserving its (repository, role) slot,
// so the lane relies on Snowcat's own claimable count again (ADR-0010-adjacent
// launch-to-claim gap: issue #17).
func (controller *Controller) confirmPendingClaims(ctx context.Context, exited []workerExit) {
	skip := make(map[string]bool, len(exited))
	for _, exit := range exited {
		skip[exit.workerID] = true
	}
	controller.mu.Lock()
	type pendingProbe struct {
		workerID string
		probe    launchProbe
	}
	pending := make([]pendingProbe, 0)
	for workerID, probe := range controller.probes {
		if probe.claimed || !probe.stabilized || skip[workerID] {
			continue
		}
		pending = append(pending, pendingProbe{workerID, probe})
	}
	controller.mu.Unlock()
	sort.Slice(pending, func(i, j int) bool { return pending[i].workerID < pending[j].workerID })
	for _, entry := range pending {
		if ctx.Err() != nil {
			return
		}
		observed, err := controller.queue.ObserveWorker(ctx, entry.probe.repository, entry.workerID)
		if err != nil || observed.Status == "unmatched" {
			continue
		}
		controller.mu.Lock()
		if probe, exists := controller.probes[entry.workerID]; exists {
			probe.claimed = true
			controller.probes[entry.workerID] = probe
		}
		controller.mu.Unlock()
	}
}

// pendingLaunchCounts reports, per repository and role, how many live launch
// probes are still waiting on their first Snowcat claim. reconcile subtracts
// these from the freshly observed claimable count so a lane does not launch a
// second worker into the gap between a worker starting and it calling
// claim_work for the one item that made it eligible.
func (controller *Controller) pendingLaunchCounts() map[string]int {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	counts := make(map[string]int, len(controller.probes))
	for _, probe := range controller.probes {
		if probe.stabilized && !probe.claimed {
			counts[laneFailureKey(probe.repository, probe.role)]++
		}
	}
	return counts
}

func (controller *Controller) failLane(probe launchProbe, detail string) {
	controller.setBackoff(probe.repository, probe.role)
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.laneFailures[laneFailureKey(probe.repository, probe.role)] = laneFailure{
		repository: probe.repository,
		role:       probe.role,
		detail:     detail,
		retryAt:    controller.now().UTC().Add(5 * time.Minute),
	}
}

func (controller *Controller) activeLaneFailures() []laneFailure {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	now := controller.now().UTC()
	active := make([]laneFailure, 0, len(controller.laneFailures))
	for key, failure := range controller.laneFailures {
		if !failure.retryAt.After(now) {
			delete(controller.laneFailures, key)
			continue
		}
		active = append(active, failure)
	}
	return active
}

func laneFailureKey(repository string, role queueview.Role) string {
	return repository + "\x00" + string(role)
}

func (controller *Controller) ensureLanePreflight(ctx context.Context, lane Lane, repositories []managedrepo.Record, expiries map[string]time.Time, eligible bool) bool {
	key := lane.Provider + "\x00" + lane.MCPServer
	expiresAt, ready := expiries[key]
	if ready && expiresAt.After(controller.now().UTC().Add(preflightRefreshWindow)) {
		return true
	}
	if !eligible || len(repositories) == 0 {
		controller.updateProvider(ProviderStatus{
			Provider: lane.Provider, MCPServer: lane.MCPServer, Status: "refresh-needed",
			Detail: "live proof will refresh when eligible work appears", ExpiresAt: expiresAt,
		})
		return false
	}
	if controller.inNamedBackoff("preflight\x00" + key) {
		return false
	}
	preflight, err := controller.preflights.Refresh(ctx, lane.Provider, lane.MCPServer, repositories[0].Repository)
	if err != nil {
		delete(expiries, key)
		controller.setNamedBackoff("preflight\x00" + key)
		controller.updateProvider(ProviderStatus{
			Provider: lane.Provider, MCPServer: lane.MCPServer, Status: StatusDegraded,
			Detail: "provider preflight refresh failed after two attempts",
		})
		return false
	}
	expiries[key] = preflight.ExpiresAt
	controller.updateProvider(ProviderStatus{
		Provider: lane.Provider, MCPServer: lane.MCPServer, Status: preflight.Status,
		Detail: preflight.Detail, ExpiresAt: preflight.ExpiresAt,
	})
	return true
}

func validateRequest(request *Request) error {
	if request.Adapter == "" {
		request.Adapter = worker.AdapterHost
	}
	if request.Adapter != worker.AdapterHost && request.Adapter != worker.AdapterOCI {
		return fmt.Errorf("%w: adapter must be host or oci", ErrInvalid)
	}
	if request.Adapter == worker.AdapterHost && request.Runtime != "" {
		return fmt.Errorf("%w: runtime is valid only for oci", ErrInvalid)
	}
	if request.Adapter == worker.AdapterOCI && request.Runtime == "" {
		request.Runtime = worker.RuntimePodman
	}
	if request.Adapter == worker.AdapterOCI && request.Runtime != worker.RuntimePodman && request.Runtime != worker.RuntimeDocker {
		return fmt.Errorf("%w: OCI runtime must be podman or docker", ErrInvalid)
	}
	if request.IntervalSeconds == 0 {
		request.IntervalSeconds = int(defaultInterval.Seconds())
	}
	interval := time.Duration(request.IntervalSeconds) * time.Second
	if interval < minimumInterval || interval > maximumInterval {
		return fmt.Errorf("%w: interval must be between %s and %s", ErrInvalid, minimumInterval, maximumInterval)
	}
	total := 0
	providers := make(map[string]string)
	for name, lane := range map[string]Lane{"discoverer": request.Discoverer, "implementer": request.Implementer, "reviewer": request.Reviewer} {
		if !campaignNameRE.MatchString(lane.Provider) || !campaignNameRE.MatchString(lane.MCPServer) || lane.Capacity < 1 || lane.Capacity > maxTotalCapacity {
			return fmt.Errorf("%w: %s provider, MCP server, and capacity 1..%d are required", ErrInvalid, name, maxTotalCapacity)
		}
		if known, exists := providers[lane.Provider]; exists && known != lane.MCPServer {
			return fmt.Errorf("%w: one provider cannot use two MCP server names in one campaign", ErrInvalid)
		}
		providers[lane.Provider] = lane.MCPServer
		total += lane.Capacity
	}
	if total > maxTotalCapacity {
		return fmt.Errorf("%w: total role capacity may not exceed %d", ErrInvalid, maxTotalCapacity)
	}
	return nil
}

func (controller *Controller) request() Request {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.record.Request
}

func (controller *Controller) lanes() []Lane {
	request := controller.request()
	return []Lane{request.Discoverer, request.Implementer, request.Reviewer}
}

func (controller *Controller) lane(role queueview.Role) Lane {
	request := controller.request()
	switch role {
	case queueview.RoleDiscoverer:
		return request.Discoverer
	case queueview.RoleImplementer:
		return request.Implementer
	default:
		return request.Reviewer
	}
}

func (controller *Controller) updateRepository(repository string, update func(*RepositoryStatus)) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	for index := range controller.record.Repositories {
		if controller.record.Repositories[index].Repository == repository {
			update(&controller.record.Repositories[index])
			controller.record.UpdatedAt = controller.now().UTC()
			controller.persistLocked()
			return
		}
	}
}

func (controller *Controller) updateProvider(status ProviderStatus) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	for index := range controller.record.Providers {
		if controller.record.Providers[index].Provider == status.Provider && controller.record.Providers[index].MCPServer == status.MCPServer {
			controller.record.Providers[index] = status
			controller.record.UpdatedAt = controller.now().UTC()
			controller.persistLocked()
			return
		}
	}
	controller.record.Providers = append(controller.record.Providers, status)
	controller.record.UpdatedAt = controller.now().UTC()
	controller.persistLocked()
}

func (controller *Controller) addWorker(workerID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.record.WorkerIDs = append(controller.record.WorkerIDs, workerID)
	controller.record.UpdatedAt = controller.now().UTC()
	controller.persistLocked()
}

func (controller *Controller) degrade(detail string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.record.Status = StatusDegraded
	controller.record.Detail = detail
	controller.record.UpdatedAt = controller.now().UTC()
	controller.persistLocked()
}

func (controller *Controller) setRunningStatus(ready bool) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.record.Status == StatusStopping {
		return
	}
	if ready {
		controller.record.Status = StatusRunning
		controller.record.Detail = "reconciling all enrolled repositories; idle lanes wait for admission or verification"
	} else {
		controller.record.Status = StatusDegraded
		controller.record.Detail = "campaign has no ready repository/provider combination"
	}
	controller.record.UpdatedAt = controller.now().UTC()
	controller.persistLocked()
}

func (controller *Controller) setReconciledStatus() {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if controller.record.Status == StatusStopping {
		return
	}
	providerBlockers := 0
	for _, provider := range controller.record.Providers {
		if provider.Status == StatusDegraded {
			providerBlockers++
		}
	}
	repositoryBlockers := 0
	for _, repository := range controller.record.Repositories {
		if repository.Status == StatusDegraded {
			repositoryBlockers++
		}
	}
	if providerBlockers == 0 && repositoryBlockers == 0 {
		controller.record.Status = StatusRunning
		controller.record.Detail = "reconciling all enrolled repositories; idle lanes wait for admission or verification"
	} else {
		blockers := make([]string, 0, 2)
		if providerBlockers > 0 {
			blockers = append(blockers, countLabel(providerBlockers, "provider"))
		}
		if repositoryBlockers > 0 {
			blockers = append(blockers, countLabel(repositoryBlockers, "repository"))
		}
		controller.record.Status = StatusDegraded
		controller.record.Detail = "reconciliation blocked by " + strings.Join(blockers, " and ") + "; ready lanes continue"
	}
	controller.record.UpdatedAt = controller.now().UTC()
	controller.persistLocked()
}

func countLabel(count int, singular string) string {
	if count == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %ss", count, singular)
}

func (controller *Controller) setBackoff(repository string, role queueview.Role) {
	controller.setNamedBackoff("launch\x00" + repository + "\x00" + string(role))
}

func (controller *Controller) setNamedBackoff(key string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.backoff[key] = controller.now().UTC().Add(5 * time.Minute)
}

func (controller *Controller) inBackoff(repository string, role queueview.Role) bool {
	return controller.inNamedBackoff("launch\x00" + repository + "\x00" + string(role))
}

func (controller *Controller) inNamedBackoff(key string) bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.backoff[key].After(controller.now().UTC())
}

func (controller *Controller) newID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := controller.random(bytes); err != nil {
		return "", err
	}
	return "campaign-" + hex.EncodeToString(bytes), nil
}

// persistLocked is the one bounded failure path for every post-start campaign
// state write. A discarded error here would leave the controller launching
// workers against state no operator can see, so a failed write halts the
// campaign instead.
//
// The caller must hold controller.mu.
func (controller *Controller) persistLocked() {
	if err := controller.write(controller.record); err != nil {
		controller.haltLocked()
	}
}

// haltLocked marks the campaign halted by a durable state persistence failure:
// the record reports the reason, and reconciliation is cancelled so no further
// worker is launched. An already stopped campaign keeps its stopped status and
// only gains the stale-state reason.
//
// The caller must hold controller.mu.
func (controller *Controller) haltLocked() {
	controller.stateFailed = true
	if isActive(controller.record.Status) {
		controller.record.Status = StatusDegraded
		controller.record.Detail = haltedDetail
	} else {
		controller.record.Detail = stoppedAfterStateFailureDetail
	}
	if controller.cancel != nil {
		controller.cancel()
	}
}

// halted reports whether a durable state persistence failure has stopped this
// campaign from launching anything further.
func (controller *Controller) halted() bool {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	return controller.stateFailed
}

func (controller *Controller) read() (Record, error) {
	content, err := os.ReadFile(controller.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return Record{}, nil
	}
	if err != nil {
		return Record{}, fmt.Errorf("read board campaign state: %w", err)
	}
	var record Record
	if err := json.Unmarshal(content, &record); err != nil {
		return Record{}, fmt.Errorf("decode board campaign state: %w", err)
	}
	return record, nil
}

func (controller *Controller) write(record Record) error {
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode board campaign state: %w", err)
	}
	content = append(content, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(controller.statePath), ".campaign-*.json")
	if err != nil {
		return fmt.Errorf("create board campaign state: %w", err)
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, controller.statePath)
}

func cloneRecord(record Record) Record {
	record.Repositories = append([]RepositoryStatus(nil), record.Repositories...)
	record.Providers = append([]ProviderStatus(nil), record.Providers...)
	record.WorkerIDs = append([]string(nil), record.WorkerIDs...)
	return record
}

func isActive(status string) bool {
	return status == StatusStarting || status == StatusRunning || status == StatusDegraded || status == StatusStopping
}
