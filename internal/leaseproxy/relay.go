package leaseproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	leaseSeconds    = 120
	renewalInterval = 30 * time.Second
	maxMessage      = 1024 * 1024
	markerVersion   = 1
	markerName      = "cockpit-lifecycle.json"
)

var (
	ErrInvalid      = errors.New("invalid worker lease relay configuration")
	ErrUnavailable  = errors.New("snowcat worker lease relay is unavailable")
	workerIDPattern = regexp.MustCompile(`^worker-[0-9a-f]{16}$`)
)

type Config struct {
	Endpoint           string
	Token              string
	AccessClientID     string
	AccessClientSecret string
	WorkerID           string
	Workspace          string
	HTTPClient         *http.Client
	Input              io.Reader
	Output             io.Writer
	Errors             io.Writer
	Now                func() time.Time
}

type Marker struct {
	Version              int       `json:"version"`
	WorkerID             string    `json:"workerId"`
	ItemID               string    `json:"itemId"`
	Status               string    `json:"status"`
	CompleteAttempted    bool      `json:"completeAttempted"`
	CompleteAcknowledged bool      `json:"completeAcknowledged"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

type lease struct {
	itemID    string
	token     string
	worker    string
	expiresAt time.Time
}

type Relay struct {
	endpoint           *url.URL
	token              string
	accessClientID     string
	accessClientSecret string
	workerID           string
	workspace          string
	client             *http.Client
	input              io.Reader
	output             io.Writer
	errors             io.Writer
	now                func() time.Time

	mutex                sync.Mutex
	lifecycleMutex       sync.Mutex
	active               *lease
	lost                 bool
	terminal             bool
	completeAttempted    bool
	completeAcknowledged bool
	requestSequence      atomic.Int64
	outputMutex          sync.Mutex
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  rpcParams       `json:"params,omitempty"`
}

type rpcParams struct {
	Name      string         `json:"name,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

type toolResult struct {
	IsError bool `json:"isError,omitempty"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}

type workItem struct {
	ID             string `json:"id"`
	LeaseToken     string `json:"leaseToken"`
	LeaseExpiresAt string `json:"leaseExpiresAt"`
}

func New(config Config) (*Relay, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, fmt.Errorf("%w: endpoint must be an absolute HTTP URL", ErrInvalid)
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: endpoint must not contain user info, a query, or a fragment", ErrInvalid)
	}
	if strings.TrimSpace(config.Token) == "" || strings.ContainsAny(config.Token, "\r\n") || !workerIDPattern.MatchString(config.WorkerID) {
		return nil, fmt.Errorf("%w: token and worker ID are required", ErrInvalid)
	}
	if (config.AccessClientID == "") != (config.AccessClientSecret == "") || strings.ContainsAny(config.AccessClientID+config.AccessClientSecret, "\r\n") {
		return nil, fmt.Errorf("%w: Cloudflare Access client ID and secret must be supplied together as single-line values", ErrInvalid)
	}
	workspace, err := filepath.Abs(config.Workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace: %v", ErrInvalid, err)
	}
	workspace, err = filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("%w: workspace: %v", ErrInvalid, err)
	}
	info, err := os.Stat(workspace)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: workspace must be an existing directory", ErrInvalid)
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		}
	}
	if config.Input == nil {
		config.Input = os.Stdin
	}
	if config.Output == nil {
		config.Output = os.Stdout
	}
	if config.Errors == nil {
		config.Errors = os.Stderr
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Relay{
		endpoint: endpoint, token: strings.TrimSpace(config.Token), workerID: config.WorkerID,
		accessClientID: config.AccessClientID, accessClientSecret: config.AccessClientSecret,
		workspace: workspace, client: config.HTTPClient, input: config.Input, output: config.Output,
		errors: config.Errors, now: config.Now,
	}, nil
}

func (relay *Relay) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewalInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				relay.renew(ctx)
			}
		}
	}()

	scanner := bufio.NewScanner(relay.input)
	scanner.Buffer(make([]byte, 64*1024), maxMessage)
	for scanner.Scan() {
		response, emit, err := relay.Handle(ctx, append([]byte(nil), scanner.Bytes()...))
		if err != nil {
			cancel()
			<-done
			return err
		}
		if emit {
			relay.outputMutex.Lock()
			_, writeErr := relay.output.Write(append(response, '\n'))
			relay.outputMutex.Unlock()
			if writeErr != nil {
				cancel()
				<-done
				return fmt.Errorf("write MCP relay response: %w", writeErr)
			}
		}
	}
	cancel()
	<-done
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read MCP relay request: %w", err)
	}
	relay.markProviderExit()
	return nil
}

func (relay *Relay) Handle(ctx context.Context, payload []byte) ([]byte, bool, error) {
	var request rpcRequest
	if err := json.Unmarshal(payload, &request); err != nil || request.JSONRPC != "2.0" || request.Method == "" {
		return nil, false, fmt.Errorf("%w: provider sent invalid JSON-RPC", ErrUnavailable)
	}
	if request.Method == "tools/call" && isLifecycleTool(request.Params.Name) {
		relay.lifecycleMutex.Lock()
		defer relay.lifecycleMutex.Unlock()
	}
	if request.Method == "tools/call" {
		if request.Params.Name == "claim_work" {
			if err := relay.prepareClaim(&request); err != nil {
				return errorResult(request.ID, err.Error()), true, nil
			}
		} else if relay.isLost() {
			return errorResult(request.ID, "SNOWCAT_COCKPIT_LEASE_LOST: stop immediately without further repository or GitHub mutation"), true, nil
		}
		if isLifecycleTool(request.Params.Name) && request.Params.Name != "claim_work" {
			relay.bindLease(&request)
		}
		if request.Params.Name == "heartbeat_work" {
			request.Params.Arguments["leaseSeconds"] = leaseSeconds
		}
		if request.Params.Name == "complete_work" {
			relay.noteCompletionAttempt(request.Params.Arguments)
		}
		var err error
		payload, err = json.Marshal(request)
		if err != nil {
			return nil, false, fmt.Errorf("encode bounded MCP request: %w", err)
		}
	}

	response, emit, err := relay.forward(ctx, payload)
	if err != nil {
		return nil, false, err
	}
	if emit && request.Method == "tools/call" {
		relay.observe(request, response)
	}
	return response, emit, nil
}

func (relay *Relay) holdsItem(id string) bool {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	return relay.active != nil && relay.active.itemID == id
}

func (relay *Relay) Renew(ctx context.Context) {
	relay.renew(ctx)
}

// bindLease substitutes the lease the relay holds — the token Snowcat minted
// on this worker's claim_work and the bound worker identity — into a
// lifecycle call, so the provider model never has to echo the token back
// correctly (a Copilot reviewer once sent a malformed one and lost a healthy
// lease). A call for an item other than the held lease is forwarded as sent
// and Snowcat judges it.
func (relay *Relay) bindLease(request *rpcRequest) {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	if relay.active == nil {
		return
	}
	if request.Params.Arguments == nil {
		request.Params.Arguments = map[string]any{}
	}
	id, _ := request.Params.Arguments["id"].(string)
	if id != "" && id != relay.active.itemID {
		return
	}
	request.Params.Arguments["id"] = relay.active.itemID
	request.Params.Arguments["leaseToken"] = relay.active.token
	request.Params.Arguments["worker"] = relay.workerID
}

func (relay *Relay) prepareClaim(request *rpcRequest) error {
	worker, _ := request.Params.Arguments["worker"].(string)
	if worker != relay.workerID {
		return errors.New("claim_work worker does not match the bound Cockpit worker")
	}
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	if relay.active != nil && !relay.terminal {
		return errors.New("cockpit worker already owns one lease")
	}
	request.Params.Arguments["leaseSeconds"] = leaseSeconds
	return nil
}

func (relay *Relay) noteCompletionAttempt(arguments map[string]any) {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	relay.completeAttempted = true
	if relay.active != nil {
		relay.writeMarkerLocked("claimed")
	}
}

func (relay *Relay) observe(request rpcRequest, response []byte) {
	item, ok := successfulWorkItem(response)
	if !ok {
		// Only a rejected renewal of the lease the relay itself holds means the
		// lease is gone; a model-issued heartbeat for some other item is not
		// evidence about this worker's lease.
		if id, _ := request.Params.Arguments["id"].(string); request.Params.Name == "heartbeat_work" && relay.holdsItem(id) {
			relay.loseLease("Snowcat rejected worker lease renewal")
		}
		return
	}
	worker, _ := request.Params.Arguments["worker"].(string)
	switch request.Params.Name {
	case "claim_work":
		if item.ID == "" || item.LeaseToken == "" || worker != relay.workerID {
			return
		}
		expiresAt, err := time.Parse(time.RFC3339, item.LeaseExpiresAt)
		if err != nil {
			relay.loseLease("Snowcat returned an invalid lease expiry")
			return
		}
		relay.mutex.Lock()
		relay.active = &lease{itemID: item.ID, token: item.LeaseToken, worker: worker, expiresAt: expiresAt}
		relay.lost = false
		relay.terminal = false
		relay.writeMarkerLocked("claimed")
		relay.mutex.Unlock()
	case "heartbeat_work":
		expiresAt, err := time.Parse(time.RFC3339, item.LeaseExpiresAt)
		if err != nil {
			relay.loseLease("Snowcat returned an invalid lease expiry")
			return
		}
		relay.mutex.Lock()
		if relay.active != nil && relay.active.itemID == item.ID {
			relay.active.expiresAt = expiresAt
		}
		relay.mutex.Unlock()
	case "complete_work", "block_work", "release_work":
		status := map[string]string{"complete_work": "completed", "block_work": "blocked", "release_work": "released"}[request.Params.Name]
		relay.mutex.Lock()
		relay.terminal = true
		if request.Params.Name == "complete_work" {
			relay.completeAcknowledged = true
		}
		relay.writeMarkerLocked(status)
		relay.mutex.Unlock()
	}
}

func (relay *Relay) renew(ctx context.Context) {
	relay.lifecycleMutex.Lock()
	defer relay.lifecycleMutex.Unlock()

	relay.mutex.Lock()
	if relay.active == nil || relay.terminal || relay.lost {
		relay.mutex.Unlock()
		return
	}
	current := *relay.active
	relay.mutex.Unlock()

	request := rpcRequest{
		JSONRPC: "2.0", ID: json.RawMessage(fmt.Sprintf("%d", relay.requestSequence.Add(1))), Method: "tools/call",
		Params: rpcParams{Name: "heartbeat_work", Arguments: map[string]any{
			"id": current.itemID, "leaseToken": current.token, "worker": current.worker, "leaseSeconds": leaseSeconds,
		}},
	}
	payload, _ := json.Marshal(request)
	response, emit, err := relay.forward(ctx, payload)
	if err != nil || !emit {
		if !relay.now().Before(current.expiresAt) {
			relay.loseLease("worker lease renewal could not reach Snowcat before expiry")
		}
		return
	}
	item, ok := successfulWorkItem(response)
	if !ok {
		relay.loseLease("Snowcat rejected worker lease renewal")
		return
	}
	expiresAt, err := time.Parse(time.RFC3339, item.LeaseExpiresAt)
	if err != nil || item.ID != current.itemID {
		relay.loseLease("Snowcat returned an invalid worker lease renewal")
		return
	}
	relay.mutex.Lock()
	if relay.active != nil && relay.active.itemID == current.itemID {
		relay.active.expiresAt = expiresAt
	}
	relay.mutex.Unlock()
}

func isLifecycleTool(name string) bool {
	switch name {
	case "claim_work", "heartbeat_work", "complete_work", "block_work", "release_work":
		return true
	default:
		return false
	}
}

func (relay *Relay) forward(ctx context.Context, payload []byte) ([]byte, bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, relay.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("build Snowcat MCP request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+relay.token)
	if relay.accessClientID != "" {
		request.Header.Set("CF-Access-Client-Id", relay.accessClientID)
		request.Header.Set("CF-Access-Client-Secret", relay.accessClientSecret)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := relay.client.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("%w: Snowcat MCP request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxMessage))
		return nil, false, nil
	}
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxMessage))
		return nil, false, fmt.Errorf("%w: Snowcat MCP returned HTTP %d", ErrUnavailable, response.StatusCode)
	}
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, maxMessage+1))
	if err != nil || len(responsePayload) > maxMessage {
		return nil, false, fmt.Errorf("%w: Snowcat MCP response exceeded the relay limit", ErrUnavailable)
	}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		responsePayload, err = firstSSEData(responsePayload)
		if err != nil {
			return nil, false, fmt.Errorf("%w: Snowcat MCP returned invalid event data", ErrUnavailable)
		}
	}
	return responsePayload, true, nil
}

func successfulWorkItem(payload []byte) (workItem, bool) {
	var envelope rpcResponse
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Error) != 0 || len(envelope.Result) == 0 {
		return workItem{}, false
	}
	var result toolResult
	if json.Unmarshal(envelope.Result, &result) != nil || result.IsError || len(result.Content) != 1 || result.Content[0].Type != "text" {
		return workItem{}, false
	}
	var item workItem
	if json.Unmarshal([]byte(result.Content[0].Text), &item) != nil {
		return workItem{}, false
	}
	return item, true
}

func (relay *Relay) isLost() bool {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	return relay.lost
}

func (relay *Relay) loseLease(detail string) {
	relay.mutex.Lock()
	if relay.lost || relay.terminal || relay.active == nil {
		relay.mutex.Unlock()
		return
	}
	relay.lost = true
	relay.writeMarkerLocked("lease-lost")
	relay.mutex.Unlock()
	fmt.Fprintf(relay.errors, "SNOWCAT_COCKPIT_LEASE_LOST: %s; stop immediately without further repository or GitHub mutation\n", detail)
}

func (relay *Relay) markProviderExit() {
	relay.mutex.Lock()
	defer relay.mutex.Unlock()
	if relay.active == nil || relay.terminal || relay.lost {
		return
	}
	relay.writeMarkerLocked("provider-exited")
}

func (relay *Relay) writeMarkerLocked(status string) {
	if relay.active == nil {
		return
	}
	marker := Marker{
		Version: markerVersion, WorkerID: relay.workerID, ItemID: relay.active.itemID, Status: status,
		CompleteAttempted: relay.completeAttempted, CompleteAcknowledged: relay.completeAcknowledged,
		UpdatedAt: relay.now().UTC(),
	}
	payload, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not encode lifecycle marker")
		return
	}
	directory := filepath.Join(relay.workspace, ".agents")
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		fmt.Fprintln(relay.errors, "cockpit lease relay: private lifecycle marker directory is unavailable")
		return
	}
	temporary, err := os.CreateTemp(directory, ".cockpit-lifecycle-*")
	if err != nil {
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not create lifecycle marker")
		return
	}
	path := temporary.Name()
	defer func() { _ = os.Remove(path) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not secure lifecycle marker")
		return
	}
	if _, err := temporary.Write(append(payload, '\n')); err != nil {
		_ = temporary.Close()
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not write lifecycle marker")
		return
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not sync lifecycle marker")
		return
	}
	if err := temporary.Close(); err != nil {
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not close lifecycle marker")
		return
	}
	if err := os.Rename(path, filepath.Join(directory, markerName)); err != nil {
		fmt.Fprintln(relay.errors, "cockpit lease relay: could not install lifecycle marker")
	}
}

func errorResult(id json.RawMessage, message string) []byte {
	response := map[string]any{
		"jsonrpc": "2.0",
		"result": map[string]any{
			"isError": true,
			"content": []map[string]string{{"type": "text", "text": message}},
		},
	}
	if len(id) != 0 {
		response["id"] = json.RawMessage(id)
	}
	payload, _ := json.Marshal(response)
	return payload
}

func firstSSEData(payload []byte) ([]byte, error) {
	lines := strings.Split(strings.ReplaceAll(string(payload), "\r\n", "\n"), "\n")
	data := make([]string, 0, 1)
	for _, line := range lines {
		if line == "" {
			if len(data) != 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) != 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, errors.New("SSE response contained no data event")
}
