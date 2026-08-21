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
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeWorkerManager struct {
	records []worker.Record
	launch  worker.LaunchRequest
}

func (manager *fakeWorkerManager) List(context.Context) ([]worker.Record, error) {
	return manager.records, nil
}

func (manager *fakeWorkerManager) Launch(_ context.Context, request worker.LaunchRequest) (worker.Record, error) {
	manager.launch = request
	if request.Provider == "blocked" {
		return worker.Record{}, worker.ErrNotReady
	}
	record := worker.Record{ID: "worker-0123456789abcdef", Status: worker.StatusRunning, Provider: request.Provider, Role: request.Role}
	manager.records = append(manager.records, record)
	return record, nil
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
	workers := &fakeWorkerManager{records: []worker.Record{{ID: "worker-fedcba9876543210", Status: worker.StatusExited}}}
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
		Workers: workers,
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
		if len(records) != 1 || records[0].Status != worker.StatusExited {
			t.Fatalf("records = %#v", records)
		}
	})

	t.Run("launch worker", func(t *testing.T) {
		body := strings.NewReader(`{"provider":"claude","role":"implementer","repository":"frostyard/firn","source":"/repo","baseRef":"HEAD"}`)
		request := httptest.NewRequest(http.MethodPost, "/api/v1/workers", body)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusCreated {
			t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
		}
		if workers.launch.Role != "implementer" || workers.launch.Repository != "frostyard/firn" {
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
