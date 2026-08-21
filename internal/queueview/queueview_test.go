package queueview

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPObserverTakesOneBoundedSnapshot(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if got := request.Header.Get("Authorization"); got != "Bearer observer-secret" {
			t.Fatalf("Authorization = %q", got)
		}
		var input rpcRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input.Method != "tools/call" || input.Params.Name != "list_work" {
			t.Fatalf("request = %#v", input)
		}
		if input.Params.Arguments.Status != "queued" || input.Params.Arguments.Repository != "frostyard/firn" || input.Params.Arguments.Limit != MaxItems {
			t.Fatalf("arguments = %#v", input.Params.Arguments)
		}
		payload, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"content": []map[string]string{{"type": "text", "text": `[
					{"id":"1","repository":"frostyard/firn","kind":"quality-gap-discovery","priority":4,"status":"queued","allowedActions":["read","create-followup"],"requiredArtifact":"none"},
					{"id":"2","repository":"frostyard/firn","kind":"ci-signal-fix","priority":3,"status":"queued","allowedActions":["read","write","run-tests","open-pr"],"requiredArtifact":"pull-request"},
					{"id":"3","repository":"frostyard/firn","kind":"pr-review","priority":2,"status":"queued","allowedActions":["read","run-tests"],"requiredArtifact":"none"},
					{"id":"4","repository":"frostyard/firn","kind":"release-needed","priority":1,"status":"queued","allowedActions":["read"],"requiredArtifact":"none"}
				]`}},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event: message\ndata: " + string(payload) + "\n\n")),
			Request:    request,
		}, nil
	})}

	now := time.Date(2026, 8, 21, 19, 0, 0, 0, time.UTC)
	observer, err := NewHTTPObserver(HTTPConfig{Endpoint: "https://snowcat.test/mcp", Token: "observer-secret", HTTPClient: client, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := observer.Observe(context.Background(), "frostyard/firn")
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls.Load())
	}
	if snapshot.ObservedAt != now || snapshot.Truncated || len(snapshot.Items) != 4 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Counts[RoleDiscoverer] != 1 || snapshot.Counts[RoleImplementer] != 1 || snapshot.Counts[RoleReviewer] != 1 || snapshot.Counts[RoleUnassigned] != 1 {
		t.Fatalf("counts = %#v", snapshot.Counts)
	}
	if snapshot.Items[1].RequiredArtifact != "pull-request" || snapshot.Items[1].Role != RoleImplementer || snapshot.Items[1].Contract != "ready" {
		t.Fatalf("implementation item = %#v", snapshot.Items[1])
	}
	if snapshot.Items[0].Contract != "ready" {
		t.Fatalf("discovery contract = %#v", snapshot.Items[0])
	}
}

func TestFirstSSEDataSupportsMultilineDataAndCRLF(t *testing.T) {
	t.Parallel()
	payload, err := firstSSEData([]byte(": keepalive\r\nevent: message\r\ndata: {\"one\":\r\ndata: 1}\r\n\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "{\"one\":\n1}" {
		t.Fatalf("payload = %q", payload)
	}
}

func TestContractAssessmentFlagsChangeWorkWithoutDeliveryAuthority(t *testing.T) {
	t.Parallel()
	status, detail := assessContract("pull-request", []string{"read", "write", "run-tests"})
	if status != "suspicious" || detail == "" {
		t.Fatalf("assessment = %q, %q", status, detail)
	}
	status, detail = assessContract("", []string{"read"})
	if status != "unknown" || detail == "" {
		t.Fatalf("legacy assessment = %q, %q", status, detail)
	}
	status, detail = assessContract("surprise", []string{"read"})
	if status != "unknown" || detail == "" {
		t.Fatalf("unknown artifact assessment = %q, %q", status, detail)
	}
}

func TestHTTPObserverDoesNotExposeRemoteErrorBodies(t *testing.T) {
	t.Parallel()
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("do-not-retain-this-server-detail")),
			Request:    request,
		}, nil
	})}

	observer, err := NewHTTPObserver(HTTPConfig{Endpoint: "https://snowcat.test/mcp", Token: "secret", HTTPClient: client})
	if err != nil {
		t.Fatal(err)
	}
	_, err = observer.Observe(context.Background(), "frostyard/firn")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "do-not-retain") {
		t.Fatalf("remote error body escaped: %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := map[string]Role{
		"docs-drift-discovery": RoleDiscoverer,
		"quality-gap-fix":      RoleImplementer,
		"pr-review-fix":        RoleImplementer,
		"pr-cure":              RoleImplementer,
		"pr-cure-change":       RoleImplementer,
		"pr-review":            RoleReviewer,
		"issue-resolution":     RoleUnassigned,
	}
	for kind, want := range tests {
		if got := Classify(kind); got != want {
			t.Errorf("Classify(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestHTTPObserverValidatesConfigurationAndRepository(t *testing.T) {
	t.Parallel()
	for _, config := range []HTTPConfig{
		{},
		{Endpoint: "file:///tmp/mcp", Token: "secret"},
		{Endpoint: "https://secret@example.com/mcp", Token: "secret"},
		{Endpoint: "https://example.com/mcp?token=nope", Token: "secret"},
		{Endpoint: "https://example.com/mcp", Token: ""},
	} {
		if _, err := NewHTTPObserver(config); !errors.Is(err, ErrInvalid) {
			t.Errorf("NewHTTPObserver(%#v) error = %v", config, err)
		}
	}
	observer, err := NewHTTPObserver(HTTPConfig{Endpoint: "https://example.com/mcp", Token: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observer.Observe(context.Background(), "not-a-repository"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Observe error = %v", err)
	}
}
