package leaseproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testWorker = "worker-0123456789abcdef"
	testItem   = "01234567-89ab-cdef-0123-456789abcdef"
	testToken  = "12345678-9abc-def0-1234-56789abcdef0"
)

type snowcatFixture struct {
	mutex              sync.Mutex
	requests           []rpcRequest
	rejectRenew        bool
	completeItem       workItem
	accessClientID     string
	accessClientSecret string
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func (fixture *snowcatFixture) client() *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := httptest.NewRecorder()
		fixture.ServeHTTP(response, request)
		return response.Result(), nil
	})}
}

func (fixture *snowcatFixture) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer worker-secret" {
		http.Error(response, "unauthorized", http.StatusUnauthorized)
		return
	}
	if fixture.accessClientID != "" && (request.Header.Get("CF-Access-Client-Id") != fixture.accessClientID || request.Header.Get("CF-Access-Client-Secret") != fixture.accessClientSecret) {
		http.Error(response, "Access service token missing", http.StatusForbidden)
		return
	}
	payload, _ := io.ReadAll(request.Body)
	var call rpcRequest
	if json.Unmarshal(payload, &call) != nil {
		http.Error(response, "bad request", http.StatusBadRequest)
		return
	}
	fixture.mutex.Lock()
	fixture.requests = append(fixture.requests, call)
	rejectRenew := fixture.rejectRenew && call.Params.Name == "heartbeat_work"
	fixture.mutex.Unlock()
	response.Header().Set("Content-Type", "application/json")
	if rejectRenew {
		_, _ = response.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"isError":true,"content":[{"type":"text","text":"lease has expired"}]}}`))
		return
	}
	item := workItem{ID: testItem, LeaseToken: testToken, LeaseExpiresAt: "2026-08-23T21:02:00Z"}
	if call.Params.Name == "complete_work" && fixture.completeItem.ID != "" {
		item = fixture.completeItem
	}
	text, _ := json.Marshal(item)
	result, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(call.ID),
		"result": map[string]any{"content": []map[string]string{{"type": "text", "text": string(text)}}, "structuredContent": map[string]any{"value": item}},
	})
	_, _ = response.Write(result)
}

func TestRelayBoundsAndRenewsLeaseAndRecordsAcknowledgedCompletion(t *testing.T) {
	t.Parallel()

	fixture := &snowcatFixture{accessClientID: "access-client-id", accessClientSecret: "access-client-secret"}
	workspace := relayWorkspace(t)
	now := time.Date(2026, 8, 23, 21, 0, 0, 0, time.UTC)
	relay, err := New(Config{
		Endpoint: "https://snowcat.test/mcp", Token: "worker-secret", WorkerID: testWorker, Workspace: workspace,
		AccessClientID: "access-client-id", AccessClientSecret: "access-client-secret",
		HTTPClient: fixture.client(),
		Now:        func() time.Time { return now }, Errors: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}

	claim := requestPayload(1, "claim_work", map[string]any{"worker": testWorker, "leaseSeconds": 3600})
	if _, emit, err := relay.Handle(context.Background(), claim); err != nil || !emit {
		t.Fatalf("claim relay = emit %v, error %v", emit, err)
	}
	relay.Renew(context.Background())
	complete := requestPayload(2, "complete_work", map[string]any{
		"id": testItem, "leaseToken": testToken, "worker": testWorker,
		"result": map[string]any{"summary": "done", "evidence": []string{"test"}, "artifacts": []any{}}, "followUps": []any{},
	})
	if _, emit, err := relay.Handle(context.Background(), complete); err != nil || !emit {
		t.Fatalf("complete relay = emit %v, error %v", emit, err)
	}

	fixture.mutex.Lock()
	requests := append([]rpcRequest(nil), fixture.requests...)
	fixture.mutex.Unlock()
	if len(requests) != 3 {
		t.Fatalf("requests = %#v", requests)
	}
	for _, index := range []int{0, 1} {
		if requests[index].Params.Arguments["leaseSeconds"] != float64(leaseSeconds) {
			t.Fatalf("request %d leaseSeconds = %#v", index, requests[index].Params.Arguments["leaseSeconds"])
		}
	}
	marker := readMarker(t, workspace)
	if marker.Status != "completed" || !marker.CompleteAttempted || !marker.CompleteAcknowledged || marker.ItemID != testItem {
		t.Fatalf("marker = %#v", marker)
	}
	if payload, err := os.ReadFile(filepath.Join(workspace, ".agents", markerName)); err != nil {
		t.Fatal(err)
	} else if bytes.Contains(payload, []byte(testToken)) || bytes.Contains(payload, []byte("worker-secret")) || bytes.Contains(payload, []byte("access-client-secret")) {
		t.Fatalf("marker contains credential material: %s", payload)
	}
}

func TestRelayRejectsIncompleteOrMultilineAccessCredentials(t *testing.T) {
	t.Parallel()

	for _, config := range []Config{
		{AccessClientID: "id-only"},
		{AccessClientSecret: "secret-only"},
		{AccessClientID: "bad\nid", AccessClientSecret: "access-secret"},
	} {
		config.Endpoint = "https://snowcat.test/mcp"
		config.Token = "worker-secret"
		config.WorkerID = testWorker
		config.Workspace = relayWorkspace(t)
		if _, err := New(config); !errors.Is(err, ErrInvalid) {
			t.Errorf("New(%#v) error = %v", config, err)
		}
	}
}

func TestRelaySignalsDefinitiveLeaseLossAndRefusesFurtherTools(t *testing.T) {
	t.Parallel()

	fixture := &snowcatFixture{}
	workspace := relayWorkspace(t)
	var errorsOutput bytes.Buffer
	relay, err := New(Config{
		Endpoint: "https://snowcat.test/mcp", Token: "worker-secret", WorkerID: testWorker, Workspace: workspace,
		HTTPClient: fixture.client(),
		Now:        func() time.Time { return time.Date(2026, 8, 23, 21, 0, 0, 0, time.UTC) }, Errors: &errorsOutput,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.Handle(context.Background(), requestPayload(1, "claim_work", map[string]any{"worker": testWorker})); err != nil {
		t.Fatal(err)
	}
	fixture.mutex.Lock()
	fixture.rejectRenew = true
	fixture.mutex.Unlock()
	relay.Renew(context.Background())
	response, emit, err := relay.Handle(context.Background(), requestPayload(2, "complete_work", map[string]any{"worker": testWorker}))
	if err != nil || !emit || !strings.Contains(string(response), "SNOWCAT_COCKPIT_LEASE_LOST") {
		t.Fatalf("post-loss response = %s, emit %v, error %v", response, emit, err)
	}
	if !strings.Contains(errorsOutput.String(), "SNOWCAT_COCKPIT_LEASE_LOST") {
		t.Fatalf("lease-loss stderr = %q", errorsOutput.String())
	}
	marker := readMarker(t, workspace)
	if marker.Status != "lease-lost" || marker.CompleteAttempted || marker.CompleteAcknowledged {
		t.Fatalf("marker = %#v", marker)
	}
	fixture.mutex.Lock()
	requestCount := len(fixture.requests)
	fixture.mutex.Unlock()
	if requestCount != 2 {
		t.Fatalf("requests after loss = %d, want claim and renewal only", requestCount)
	}
}

func TestRelayRecordsCompletionAttemptWithoutInventingAcknowledgement(t *testing.T) {
	t.Parallel()

	fixture := &snowcatFixture{}
	workspace := relayWorkspace(t)
	relay, err := New(Config{
		Endpoint: "https://snowcat.test/mcp", Token: "worker-secret", WorkerID: testWorker, Workspace: workspace,
		HTTPClient: fixture.client(), Now: func() time.Time { return time.Date(2026, 8, 23, 21, 0, 0, 0, time.UTC) }, Errors: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := relay.Handle(context.Background(), requestPayload(1, "claim_work", map[string]any{"worker": testWorker})); err != nil {
		t.Fatal(err)
	}
	relay.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	if _, _, err := relay.Handle(context.Background(), requestPayload(2, "complete_work", map[string]any{"worker": testWorker})); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("completion error = %v", err)
	}
	marker := readMarker(t, workspace)
	if marker.Status != "claimed" || !marker.CompleteAttempted || marker.CompleteAcknowledged {
		t.Fatalf("marker = %#v", marker)
	}
}

func TestRelayRejectsAClaimForAnotherWorkerWithoutForwarding(t *testing.T) {
	t.Parallel()

	fixture := &snowcatFixture{}
	relay, err := New(Config{Endpoint: "https://snowcat.test/mcp", Token: "worker-secret", WorkerID: testWorker, Workspace: relayWorkspace(t), HTTPClient: fixture.client(), Errors: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	response, emit, err := relay.Handle(context.Background(), requestPayload(1, "claim_work", map[string]any{"worker": "worker-fedcba9876543210"}))
	if err != nil || !emit || !strings.Contains(string(response), "does not match") {
		t.Fatalf("claim response = %s, emit %v, error %v", response, emit, err)
	}
	fixture.mutex.Lock()
	defer fixture.mutex.Unlock()
	if len(fixture.requests) != 0 {
		t.Fatalf("mismatched claim was forwarded: %#v", fixture.requests)
	}
}

func requestPayload(id int, name string, arguments map[string]any) []byte {
	payload, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(strconv.Itoa(id)), Method: "tools/call", Params: rpcParams{Name: name, Arguments: arguments}})
	return payload
}

func readMarker(t *testing.T, workspace string) Marker {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(workspace, ".agents", markerName))
	if err != nil {
		t.Fatal(err)
	}
	var marker Marker
	if err := json.Unmarshal(payload, &marker); err != nil {
		t.Fatal(err)
	}
	return marker
}

func relayWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, ".agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func TestRelayBindsItsOwnLeaseTokenOnLifecycleCalls(t *testing.T) {
	t.Parallel()

	fixture := &snowcatFixture{}
	relay, err := New(Config{
		Endpoint: "https://snowcat.test/mcp", Token: "worker-secret", WorkerID: testWorker, Workspace: relayWorkspace(t),
		HTTPClient: fixture.client(),
		Now:        func() time.Time { return time.Date(2026, 8, 24, 17, 0, 0, 0, time.UTC) }, Errors: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	claim := requestPayload(1, "claim_work", map[string]any{"worker": testWorker})
	if _, emit, err := relay.Handle(context.Background(), claim); err != nil || !emit {
		t.Fatalf("claim relay = emit %v, error %v", emit, err)
	}

	// The model echoes a mangled token and omits the worker; the forwarded
	// heartbeat carries the lease the relay holds, and the lease survives.
	heartbeat := requestPayload(2, "heartbeat_work", map[string]any{"id": testItem, "leaseToken": "not-a-uuid"})
	if _, emit, err := relay.Handle(context.Background(), heartbeat); err != nil || !emit {
		t.Fatalf("heartbeat relay = emit %v, error %v", emit, err)
	}
	fixture.mutex.Lock()
	forwarded := fixture.requests[len(fixture.requests)-1]
	fixture.mutex.Unlock()
	if forwarded.Params.Arguments["leaseToken"] != testToken || forwarded.Params.Arguments["worker"] != testWorker || forwarded.Params.Arguments["id"] != testItem {
		t.Fatalf("forwarded heartbeat did not carry the relay's lease: %#v", forwarded.Params.Arguments)
	}
	if relay.isLost() {
		t.Fatal("a mangled model token lost a healthy lease")
	}

	// A rejected heartbeat for some other item is not evidence about this lease.
	fixture.mutex.Lock()
	fixture.rejectRenew = true
	fixture.mutex.Unlock()
	other := requestPayload(3, "heartbeat_work", map[string]any{"id": "00000000-0000-4000-8000-000000000000", "leaseToken": "not-a-uuid"})
	if _, _, err := relay.Handle(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if relay.isLost() {
		t.Fatal("a rejected heartbeat for another item lost this lease")
	}
}
