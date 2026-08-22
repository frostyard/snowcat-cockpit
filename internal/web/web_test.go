package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/doctor"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeWorkerManager struct {
	records  []worker.Record
	launch   worker.LaunchRequest
	launches []worker.LaunchRequest
}

func (manager *fakeWorkerManager) InspectBase(_ context.Context, source, baseRef string) (worker.BaseInspection, error) {
	return worker.BaseInspection{
		Source: source, BaseRef: baseRef, BaseCommit: strings.Repeat("a", 40), Upstream: "origin/main",
		Status: "behind", Behind: 2, Detail: "main is 0 ahead and 2 behind local upstream origin/main; Cockpit did not fetch",
	}, nil
}

func (manager *fakeWorkerManager) List(context.Context) ([]worker.Record, error) {
	return manager.records, nil
}

func (manager *fakeWorkerManager) Get(_ context.Context, workerID string) (worker.Record, error) {
	for _, record := range manager.records {
		if record.ID == workerID {
			return record, nil
		}
	}
	return worker.Record{}, worker.ErrNotFound
}

func (manager *fakeWorkerManager) Launch(_ context.Context, request worker.LaunchRequest) (worker.Record, error) {
	manager.launch = request
	manager.launches = append(manager.launches, request)
	if request.Provider == "blocked" {
		return worker.Record{}, worker.ErrNotReady
	}
	record := worker.Record{ID: "worker-0123456789abcdef", Status: worker.StatusRunning, Provider: request.Provider, Role: request.Role}
	manager.records = append(manager.records, record)
	return record, nil
}

type fakeQueueObserver struct {
	calls      int
	repository string
	snapshot   queueview.Snapshot
}

func (observer *fakeQueueObserver) Observe(_ context.Context, repository string) (queueview.Snapshot, error) {
	observer.calls++
	observer.repository = repository
	return observer.snapshot, nil
}

func (observer *fakeQueueObserver) ObserveWorker(_ context.Context, repository, workerID string) (queueview.WorkerObservation, error) {
	observer.calls++
	observer.repository = repository
	return queueview.WorkerObservation{
		WorkerID: workerID, Repository: repository, Status: "completed",
		Detail: "Snowcat reports this worker attempt as completed", ItemID: "item-1", Kind: "ci-signal-fix", ItemStatus: "completed",
	}, nil
}

func (manager *fakeWorkerManager) Stop(_ context.Context, workerID string) (worker.Record, error) {
	if workerID == "worker-missing" {
		return worker.Record{}, worker.ErrNotFound
	}
	return worker.Record{ID: workerID, Status: worker.StatusStopped}, nil
}

func (manager *fakeWorkerManager) Cleanup(_ context.Context, workerID string) (worker.Record, error) {
	return worker.Record{ID: workerID, Status: worker.StatusCleaned}, nil
}

func (manager *fakeWorkerManager) OpenConsole(_ context.Context, workerID string) (worker.Console, error) {
	if workerID == "worker-missing" {
		return worker.Console{}, worker.ErrNotFound
	}
	return worker.Console{URL: "http://127.0.0.1:19000/"}, nil
}

func TestRoutes(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	workers := &fakeWorkerManager{records: []worker.Record{{ID: "worker-fedcba9876543210", Status: worker.StatusExited, Repository: "frostyard/firn"}}}
	queue := &fakeQueueObserver{snapshot: queueview.Snapshot{
		Repository: "frostyard/firn",
		ObservedAt: startedAt,
		Counts: map[queueview.Role]int{
			queueview.RoleDiscoverer:  1,
			queueview.RoleImplementer: 2,
			queueview.RoleReviewer:    1,
			queueview.RoleUnassigned:  0,
		},
		Items: []queueview.Item{{ID: "item-1", Repository: "frostyard/firn", Kind: "ci-signal-fix", Role: queueview.RoleImplementer}},
	}}
	handler := New(Config{
		NodeID:    "node-0123456789abcdef0123456789abcdef",
		Version:   "test",
		StartedAt: startedAt,
		Doctor: func() doctor.Result {
			return doctor.Result{Status: doctor.StatusReady}
		},
		Profiles: func() profile.Snapshot {
			return profile.Snapshot{Status: profile.StatusPreflightRequired}
		},
		Workers:  workers,
		Queue:    queue,
		Attempts: queue,
	})

	t.Run("queue snapshot", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/queue/snapshot", strings.NewReader(`{"repository":"frostyard/firn"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var snapshot queueview.Snapshot
		if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Counts[queueview.RoleImplementer] != 2 || queue.repository != "frostyard/firn" {
			t.Fatalf("snapshot = %#v, repository = %q", snapshot, queue.repository)
		}
	})

	t.Run("base inspection", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers/base", strings.NewReader(`{"source":"/repo","baseRef":"main"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var inspection worker.BaseInspection
		if err := json.NewDecoder(response.Body).Decode(&inspection); err != nil {
			t.Fatal(err)
		}
		if inspection.Status != "behind" || inspection.Behind != 2 || inspection.Upstream != "origin/main" {
			t.Fatalf("inspection = %#v", inspection)
		}
	})

	t.Run("bounded fleet launch", func(t *testing.T) {
		before := len(workers.launches)
		body := strings.NewReader(`{"adapter":"oci","provider":"codex","role":"implementer","repository":"frostyard/firn","source":"/repo","baseRef":"main","count":9}`)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/fleets", body)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var result struct {
			Eligible int             `json:"eligible"`
			Planned  int             `json:"planned"`
			Launched []worker.Record `json:"launched"`
		}
		if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result.Eligible != 2 || result.Planned != 2 || len(result.Launched) != 2 || len(workers.launches)-before != 2 {
			t.Fatalf("result = %#v, launches = %d", result, len(workers.launches)-before)
		}
		if workers.launches[before].Adapter != worker.AdapterOCI {
			t.Fatalf("fleet adapter = %q", workers.launches[before].Adapter)
		}
	})

	t.Run("dashboard", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
		if !strings.Contains(response.Body.String(), "Snowcat cockpit") {
			t.Fatal("dashboard body is missing")
		}
		if !strings.Contains(response.Body.String(), "Launch one discoverer") {
			t.Fatal("dashboard discoverer control is missing")
		}
		if !strings.Contains(response.Body.String(), "Observe once") || !strings.Contains(response.Body.String(), "Launch implementer fleet") {
			t.Fatal("dashboard queue or bounded-fleet control is missing")
		}
		if strings.Contains(response.Body.String(), "https://") || strings.Contains(response.Body.String(), "http://") {
			t.Fatal("dashboard contains an external runtime dependency")
		}
		if got := response.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
	})

	t.Run("health", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var health Health
		if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
			t.Fatal(err)
		}
		if health.NodeID != "node-0123456789abcdef0123456789abcdef" {
			t.Fatalf("node ID = %q", health.NodeID)
		}
	})

	t.Run("profiles", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/profiles", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var snapshot profile.Snapshot
		if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Status != profile.StatusPreflightRequired {
			t.Fatalf("status = %q", snapshot.Status)
		}
	})

	t.Run("workers", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/workers", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		var records []worker.Record
		if err := json.NewDecoder(response.Body).Decode(&records); err != nil {
			t.Fatal(err)
		}
		foundExited := false
		for _, record := range records {
			foundExited = foundExited || record.ID == "worker-fedcba9876543210" && record.Status == worker.StatusExited
		}
		if !foundExited {
			t.Fatalf("records = %#v", records)
		}
	})

	t.Run("observe exact worker attempt", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers/worker-fedcba9876543210/observe", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		var observation queueview.WorkerObservation
		if err := json.NewDecoder(response.Body).Decode(&observation); err != nil {
			t.Fatal(err)
		}
		if observation.Status != "completed" || observation.WorkerID != "worker-fedcba9876543210" || observation.ItemID != "item-1" {
			t.Fatalf("observation = %#v", observation)
		}
	})

	t.Run("launch worker", func(t *testing.T) {
		body := strings.NewReader(`{"adapter":"oci","provider":"codex","role":"implementer","repository":"frostyard/firn","source":"/repo","baseRef":"HEAD"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers", body)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if workers.launch.Adapter != worker.AdapterOCI || workers.launch.Role != "implementer" || workers.launch.Repository != "frostyard/firn" {
			t.Fatalf("launch = %#v", workers.launch)
		}
	})

	t.Run("launch requires JSON", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers", strings.NewReader("provider=claude"))
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("stop worker", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers/worker-0123456789abcdef/stop", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("cleanup worker", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodDelete, "/api/v1/workers/worker-0123456789abcdef", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
	})

	t.Run("open worker console", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers/worker-0123456789abcdef/console", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d", response.Code)
		}
		var console worker.Console
		if err := json.NewDecoder(response.Body).Decode(&console); err != nil {
			t.Fatal(err)
		}
		if console.URL != "http://127.0.0.1:19000/" {
			t.Fatalf("console = %#v", console)
		}
	})

	t.Run("unknown API", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/missing", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q", got)
		}
	})

	t.Run("API root", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/api", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
			t.Fatalf("Content-Type = %q", got)
		}
	})

	t.Run("unknown page", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/missing", nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", response.Code)
		}
	})
}
