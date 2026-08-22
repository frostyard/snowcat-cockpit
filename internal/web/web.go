package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/doctor"
	"github.com/frostyard/snowcat-cockpit/internal/fleet"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

//go:embed assets/index.html
var indexHTML []byte

//go:embed assets/styles.css
var stylesCSS []byte

//go:embed assets/app.js
var appJS []byte

type Health struct {
	Status    string    `json:"status"`
	NodeID    string    `json:"nodeId"`
	StartedAt time.Time `json:"startedAt"`
	Version   string    `json:"version"`
}

type Config struct {
	NodeID    string
	Version   string
	StartedAt time.Time
	Doctor    func() doctor.Result
	Profiles  func() profile.Snapshot
	Workers   WorkerManager
	Queue     queueview.Observer
	Attempts  queueview.AttemptObserver
}

type WorkerManager interface {
	List(context.Context) ([]worker.Record, error)
	Get(context.Context, string) (worker.Record, error)
	Launch(context.Context, worker.LaunchRequest) (worker.Record, error)
	Stop(context.Context, string) (worker.Record, error)
	Cleanup(context.Context, string) (worker.Record, error)
	OpenConsole(context.Context, string) (worker.Console, error)
}

func New(config Config) http.Handler {
	mux := http.NewServeMux()
	fleets := fleet.New(config.Queue, config.Workers)
	mux.HandleFunc("GET /", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/" {
			http.NotFound(response, request)
			return
		}
		writeAsset(response, "text/html; charset=utf-8", indexHTML)
	})
	mux.HandleFunc("GET /assets/styles.css", func(response http.ResponseWriter, _ *http.Request) {
		writeAsset(response, "text/css; charset=utf-8", stylesCSS)
	})
	mux.HandleFunc("GET /assets/app.js", func(response http.ResponseWriter, _ *http.Request) {
		writeAsset(response, "text/javascript; charset=utf-8", appJS)
	})
	mux.HandleFunc("GET /api/v1/health", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, Health{
			Status:    "ok",
			NodeID:    config.NodeID,
			StartedAt: config.StartedAt,
			Version:   config.Version,
		})
	})
	mux.HandleFunc("GET /api/v1/doctor", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, config.Doctor())
	})
	mux.HandleFunc("GET /api/v1/profiles", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusOK, config.Profiles())
	})
	mux.HandleFunc("GET /api/v1/workers", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		records, err := config.Workers.List(request.Context())
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, records)
	})
	mux.HandleFunc("POST /api/v1/queue/snapshot", func(response http.ResponseWriter, request *http.Request) {
		if config.Queue == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "Snowcat queue observation is not configured"})
			return
		}
		var input struct {
			Repository string `json:"repository"`
		}
		if !decodeJSON(response, request, &input) {
			return
		}
		snapshot, err := config.Queue.Observe(request.Context(), input.Repository)
		if err != nil {
			writeQueueError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, snapshot)
	})
	mux.HandleFunc("POST /api/v1/fleets", func(response http.ResponseWriter, request *http.Request) {
		var input fleet.Request
		if !decodeJSON(response, request, &input) {
			return
		}
		result, err := fleets.Launch(request.Context(), input)
		if err != nil {
			writeQueueError(response, err)
			return
		}
		status := http.StatusCreated
		if result.Planned == 0 {
			status = http.StatusOK
		} else if len(result.Failures) != 0 {
			status = http.StatusMultiStatus
		}
		writeJSON(response, status, result)
	})
	mux.HandleFunc("POST /api/v1/workers", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		var launch worker.LaunchRequest
		if !decodeJSON(response, request, &launch) {
			return
		}
		record, err := config.Workers.Launch(request.Context(), launch)
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, record)
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/observe", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil || config.Attempts == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "Snowcat worker observation is unavailable"})
			return
		}
		record, err := config.Workers.Get(request.Context(), request.PathValue("id"))
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		observation, err := config.Attempts.ObserveWorker(request.Context(), record.Repository, record.ID)
		if err != nil {
			writeQueueError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, observation)
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/stop", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		record, err := config.Workers.Stop(request.Context(), request.PathValue("id"))
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, record)
	})
	mux.HandleFunc("POST /api/v1/workers/{id}/console", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		console, err := config.Workers.OpenConsole(request.Context(), request.PathValue("id"))
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, console)
	})
	mux.HandleFunc("DELETE /api/v1/workers/{id}", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		record, err := config.Workers.Cleanup(request.Context(), request.PathValue("id"))
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusOK, record)
	})
	mux.HandleFunc("GET /api", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	mux.HandleFunc("GET /api/", func(response http.ResponseWriter, _ *http.Request) {
		writeJSON(response, http.StatusNotFound, map[string]string{"error": "not found"})
	})
	return mux
}

func decodeJSON(response http.ResponseWriter, request *http.Request, target any) bool {
	if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
		writeJSON(response, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
		return false
	}
	decoder := json.NewDecoder(io.LimitReader(request.Body, 64*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid JSON request"})
		return false
	}
	return true
}

func writeQueueError(response http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	detail := "Snowcat queue operation failed"
	switch {
	case errors.Is(err, queueview.ErrInvalid), errors.Is(err, fleet.ErrInvalid):
		status = http.StatusBadRequest
		detail = err.Error()
	case errors.Is(err, queueview.ErrUnavailable), errors.Is(err, fleet.ErrUnavailable):
		status = http.StatusServiceUnavailable
		detail = err.Error()
	}
	writeJSON(response, status, map[string]string{"error": detail})
}

func writeWorkerError(response http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, worker.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, worker.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, worker.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, worker.ErrNotReady):
		status = http.StatusPreconditionFailed
	}
	detail := "managed-worker operation failed"
	if status != http.StatusInternalServerError {
		detail = err.Error()
	}
	writeJSON(response, status, map[string]string{"error": fmt.Sprintf("%s", detail)})
}

func writeAsset(response http.ResponseWriter, contentType string, content []byte) {
	response.Header().Set("Content-Type", contentType)
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'")
	response.Header().Set("Referrer-Policy", "no-referrer")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = response.Write(content)
}

func writeJSON(response http.ResponseWriter, status int, value any) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	response.Header().Set("X-Content-Type-Options", "nosniff")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(value)
}
