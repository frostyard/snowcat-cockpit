package fleet

import (
	"context"
	"errors"
	"testing"

	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeObserver struct {
	calls      int
	repository string
	snapshot   queueview.Snapshot
	err        error
}

func (observer *fakeObserver) Observe(_ context.Context, repository string) (queueview.Snapshot, error) {
	observer.calls++
	observer.repository = repository
	return observer.snapshot, observer.err
}

type fakeLauncher struct {
	requests []worker.LaunchRequest
	failAt   int
}

func (launcher *fakeLauncher) Launch(_ context.Context, request worker.LaunchRequest) (worker.Record, error) {
	launcher.requests = append(launcher.requests, request)
	if launcher.failAt == len(launcher.requests) {
		return worker.Record{}, errors.New("provider output that must not escape")
	}
	return worker.Record{ID: "worker-test", Provider: request.Provider, Role: request.Role}, nil
}

func TestLaunchUsesOneSnapshotAndCapsTheFleetToEligibleWork(t *testing.T) {
	t.Parallel()
	observer := &fakeObserver{snapshot: queueview.Snapshot{Counts: map[queueview.Role]int{queueview.RoleImplementer: 2}}}
	launcher := &fakeLauncher{}
	result, err := New(observer, launcher).Launch(context.Background(), Request{
		Adapter: worker.AdapterOCI, Runtime: worker.RuntimeDocker, Provider: "codex", Role: "implementer", Repository: "frostyard/firn", Source: "/repo", BaseRef: "main", Count: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if observer.calls != 1 || observer.repository != "frostyard/firn" {
		t.Fatalf("observer calls = %d, repository = %q", observer.calls, observer.repository)
	}
	if result.Eligible != 2 || result.Planned != 2 || len(result.Launched) != 2 || len(launcher.requests) != 2 {
		t.Fatalf("result = %#v, requests = %#v", result, launcher.requests)
	}
	if result.Adapter != worker.AdapterOCI || launcher.requests[0].Adapter != worker.AdapterOCI || launcher.requests[1].Adapter != worker.AdapterOCI {
		t.Fatalf("OCI adapter was not preserved: result = %#v, requests = %#v", result, launcher.requests)
	}
	if result.Runtime != worker.RuntimeDocker || launcher.requests[0].Runtime != worker.RuntimeDocker || launcher.requests[1].Runtime != worker.RuntimeDocker {
		t.Fatalf("Docker runtime was not preserved: result = %#v, requests = %#v", result, launcher.requests)
	}
}

func TestLaunchStopsAfterTheFirstFailureAndSanitizesIt(t *testing.T) {
	t.Parallel()
	observer := &fakeObserver{snapshot: queueview.Snapshot{Counts: map[queueview.Role]int{queueview.RoleReviewer: 3}}}
	launcher := &fakeLauncher{failAt: 2}
	result, err := New(observer, launcher).Launch(context.Background(), Request{
		Provider: "copilot", Role: "reviewer", Repository: "frostyard/firn", Source: "/repo", Count: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Launched) != 1 || len(result.Failures) != 1 || len(launcher.requests) != 2 {
		t.Fatalf("result = %#v, requests = %#v", result, launcher.requests)
	}
	if result.Failures[0].Detail == "provider output that must not escape" {
		t.Fatalf("raw launch error escaped: %#v", result.Failures[0])
	}
}

func TestLaunchValidatesBeforeObservation(t *testing.T) {
	t.Parallel()
	observer := &fakeObserver{}
	controller := New(observer, &fakeLauncher{})
	for _, request := range []Request{
		{Provider: "codex", Role: "implementer", Repository: "frostyard/firn", Source: "/repo", Count: 0},
		{Provider: "codex", Role: "anything", Repository: "frostyard/firn", Source: "/repo", Count: 1},
		{Provider: "", Role: "reviewer", Repository: "frostyard/firn", Source: "/repo", Count: 1},
		{Adapter: "automatic", Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: "/repo", Count: 1},
		{Adapter: worker.AdapterHost, Runtime: worker.RuntimeDocker, Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: "/repo", Count: 1},
		{Adapter: worker.AdapterOCI, Runtime: "automatic", Provider: "codex", Role: "reviewer", Repository: "frostyard/firn", Source: "/repo", Count: 1},
	} {
		if _, err := controller.Launch(context.Background(), request); !errors.Is(err, ErrInvalid) {
			t.Errorf("Launch(%#v) error = %v", request, err)
		}
	}
	if observer.calls != 0 {
		t.Fatalf("invalid requests observed Snowcat %d times", observer.calls)
	}
}
