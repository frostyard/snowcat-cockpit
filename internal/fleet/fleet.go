package fleet

import (
	"context"
	"errors"
	"fmt"

	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

const MaxWorkers = 12

var (
	ErrInvalid     = errors.New("invalid fleet launch request")
	ErrUnavailable = errors.New("fleet launch is unavailable")
)

type Request struct {
	Adapter    string `json:"adapter,omitempty"`
	Provider   string `json:"provider"`
	Role       string `json:"role"`
	Repository string `json:"repository"`
	Source     string `json:"source"`
	BaseRef    string `json:"baseRef,omitempty"`
	Count      int    `json:"count"`
}

type Failure struct {
	Ordinal int    `json:"ordinal"`
	Detail  string `json:"detail"`
}

type Result struct {
	Adapter    string             `json:"adapter"`
	Repository string             `json:"repository"`
	Role       string             `json:"role"`
	Provider   string             `json:"provider"`
	Requested  int                `json:"requested"`
	Eligible   int                `json:"eligible"`
	Planned    int                `json:"planned"`
	Launched   []worker.Record    `json:"launched"`
	Failures   []Failure          `json:"failures"`
	Snapshot   queueview.Snapshot `json:"snapshot"`
}

type WorkerLauncher interface {
	Launch(context.Context, worker.LaunchRequest) (worker.Record, error)
}

type Controller struct {
	observer queueview.Observer
	workers  WorkerLauncher
}

func New(observer queueview.Observer, workers WorkerLauncher) *Controller {
	return &Controller{observer: observer, workers: workers}
}

func (controller *Controller) Launch(ctx context.Context, request Request) (Result, error) {
	if request.Adapter == "" {
		request.Adapter = worker.AdapterHost
	}
	if request.Adapter != worker.AdapterHost && request.Adapter != worker.AdapterOCI {
		return Result{}, fmt.Errorf("%w: adapter must be host or oci", ErrInvalid)
	}
	role := queueview.Role(request.Role)
	if request.Count < 1 || request.Count > MaxWorkers {
		return Result{}, fmt.Errorf("%w: count must be between 1 and %d", ErrInvalid, MaxWorkers)
	}
	if role != queueview.RoleDiscoverer && role != queueview.RoleImplementer && role != queueview.RoleReviewer {
		return Result{}, fmt.Errorf("%w: role must be discoverer, implementer, or reviewer", ErrInvalid)
	}
	if request.Provider == "" || request.Repository == "" || request.Source == "" {
		return Result{}, fmt.Errorf("%w: provider, repository, and source are required", ErrInvalid)
	}
	if controller.observer == nil || controller.workers == nil {
		return Result{}, ErrUnavailable
	}

	snapshot, err := controller.observer.Observe(ctx, request.Repository)
	if err != nil {
		return Result{}, err
	}
	eligible := snapshot.Counts[role]
	planned := min(request.Count, eligible)
	result := Result{
		Adapter:    request.Adapter,
		Repository: request.Repository,
		Role:       request.Role,
		Provider:   request.Provider,
		Requested:  request.Count,
		Eligible:   eligible,
		Planned:    planned,
		Launched:   make([]worker.Record, 0, planned),
		Failures:   []Failure{},
		Snapshot:   snapshot,
	}
	for ordinal := 1; ordinal <= planned; ordinal++ {
		record, launchErr := controller.workers.Launch(ctx, worker.LaunchRequest{
			Adapter:    request.Adapter,
			Provider:   request.Provider,
			Role:       request.Role,
			Repository: request.Repository,
			Source:     request.Source,
			BaseRef:    request.BaseRef,
		})
		if launchErr != nil {
			result.Failures = append(result.Failures, Failure{Ordinal: ordinal, Detail: "managed-worker launch failed; earlier workspaces remain retained"})
			break
		}
		result.Launched = append(result.Launched, record)
	}
	return result, nil
}
