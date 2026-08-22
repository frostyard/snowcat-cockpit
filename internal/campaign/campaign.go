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
}

type Config struct {
	StateDirectory string
	Repositories   RepositoryCatalog
	Preflights     Preflighter
	Queue          queueview.Observer
	Workers        WorkerManager
	Now            func() time.Time
	Random         func([]byte) (int, error)
}

type Controller struct {
	statePath    string
	repositories RepositoryCatalog
	preflights   Preflighter
	queue        queueview.Observer
	workers      WorkerManager
	now          func() time.Time
	random       func([]byte) (int, error)

	mu      sync.Mutex
	record  Record
	cancel  context.CancelFunc
	done    chan struct{}
	backoff map[string]time.Time
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
		statePath:    filepath.Join(config.StateDirectory, "campaign.json"),
		repositories: config.Repositories,
		preflights:   config.Preflights,
		queue:        config.Queue,
		workers:      config.Workers,
		now:          config.Now,
		random:       config.Random,
		backoff:      make(map[string]time.Time),
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
	runContext, cancel := context.WithCancel(context.Background())
	controller.cancel = cancel
	controller.done = make(chan struct{})
	controller.backoff = make(map[string]time.Time)
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
	_ = controller.write(controller.record)
	cancel := controller.cancel
	record := cloneRecord(controller.record)
	controller.mu.Unlock()
	if cancel != nil {
		cancel()
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

func (controller *Controller) run(ctx context.Context) {
	defer func() {
		controller.mu.Lock()
		now := controller.now().UTC()
		controller.record.Status = StatusStopped
		controller.record.Detail = "board campaign stopped; managed workers and workspaces remain retained"
		controller.record.UpdatedAt = now
		controller.record.StoppedAt = &now
		_ = controller.write(controller.record)
		controller.cancel = nil
		close(controller.done)
		controller.mu.Unlock()
	}()

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
	if ctx.Err() != nil {
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
			state.Detail = fmt.Sprintf("queued: %d discoverer, %d implementer, %d reviewer", observed.snapshot.Counts[queueview.RoleDiscoverer], observed.snapshot.Counts[queueview.RoleImplementer], observed.snapshot.Counts[queueview.RoleReviewer])
			state.ObservedAt = observed.snapshot.ObservedAt
		})
	}

	for _, role := range []queueview.Role{queueview.RoleDiscoverer, queueview.RoleImplementer, queueview.RoleReviewer} {
		lane := controller.lane(role)
		eligible := 0
		for _, observed := range byRepository {
			if observed.err == nil {
				eligible += observed.snapshot.Counts[role]
			}
		}
		if !controller.ensureLanePreflight(ctx, lane, repositories, expiries, eligible > 0) {
			continue
		}
		remaining := lane.Capacity - active[role]
		for remaining > 0 {
			launchedThisPass := false
			for _, repository := range repositories {
				if remaining == 0 {
					break
				}
				observed, ok := byRepository[repository.Repository]
				if !ok || observed.err != nil || observed.snapshot.Counts[role] <= 0 || controller.inBackoff(repository.Repository, role) {
					continue
				}
				record, launchErr := controller.workers.Launch(ctx, worker.LaunchRequest{
					Adapter: controller.request().Adapter, Runtime: controller.request().Runtime,
					Provider: lane.Provider, Role: string(role), Repository: repository.Repository,
					Source: repository.Source, BaseRef: repository.BaseCommit,
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
	controller.setRunningStatus(true)
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
			_ = controller.write(controller.record)
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
			_ = controller.write(controller.record)
			return
		}
	}
	controller.record.Providers = append(controller.record.Providers, status)
	controller.record.UpdatedAt = controller.now().UTC()
	_ = controller.write(controller.record)
}

func (controller *Controller) addWorker(workerID string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.record.WorkerIDs = append(controller.record.WorkerIDs, workerID)
	controller.record.UpdatedAt = controller.now().UTC()
	_ = controller.write(controller.record)
}

func (controller *Controller) degrade(detail string) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.record.Status = StatusDegraded
	controller.record.Detail = detail
	controller.record.UpdatedAt = controller.now().UTC()
	_ = controller.write(controller.record)
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
	_ = controller.write(controller.record)
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
