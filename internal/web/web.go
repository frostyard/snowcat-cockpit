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
	"github.com/frostyard/snowcat-cockpit/internal/profile"
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
}

type WorkerManager interface {
	List(context.Context) ([]worker.Record, error)
	Launch(context.Context, worker.LaunchRequest) (worker.Record, error)
	Stop(context.Context, string) (worker.Record, error)
	Cleanup(context.Context, string) (worker.Record, error)
	OpenConsole(context.Context, string) (worker.Console, error)
}

func New(config Config) http.Handler {
	mux := http.NewServeMux()
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
	mux.HandleFunc("POST /api/v1/workers", func(response http.ResponseWriter, request *http.Request) {
		if config.Workers == nil {
			writeJSON(response, http.StatusServiceUnavailable, map[string]string{"error": "managed workers are unavailable"})
			return
		}
		if !strings.HasPrefix(strings.ToLower(request.Header.Get("Content-Type")), "application/json") {
			writeJSON(response, http.StatusUnsupportedMediaType, map[string]string{"error": "Content-Type must be application/json"})
			return
		}
		decoder := json.NewDecoder(io.LimitReader(request.Body, 64*1024))
		decoder.DisallowUnknownFields()
		var launch worker.LaunchRequest
		if err := decoder.Decode(&launch); err != nil {
			writeJSON(response, http.StatusBadRequest, map[string]string{"error": "invalid launch request"})
			return
		}
		record, err := config.Workers.Launch(request.Context(), launch)
		if err != nil {
			writeWorkerError(response, err)
			return
		}
		writeJSON(response, http.StatusCreated, record)
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
