package queueview

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const MaxItems = 100

const maxCorrelationItems = 2

var (
	ErrInvalid     = errors.New("invalid queue observation request")
	ErrUnavailable = errors.New("snowcat queue observation is unavailable")
	repositoryRE   = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	workerLabelRE  = regexp.MustCompile(`^worker-[0-9a-f]{16}$`)
)

type Role string

const (
	RoleDiscoverer  Role = "discoverer"
	RoleImplementer Role = "implementer"
	RoleReviewer    Role = "reviewer"
	RoleUnassigned  Role = "unassigned"
)

type Item struct {
	ID               string   `json:"id"`
	Repository       string   `json:"repository"`
	Kind             string   `json:"kind"`
	Priority         int      `json:"priority"`
	AllowedActions   []string `json:"allowedActions"`
	RequiredArtifact string   `json:"requiredArtifact"`
	Contract         string   `json:"contract"`
	ContractDetail   string   `json:"contractDetail,omitempty"`
	Role             Role     `json:"role"`
}

type Snapshot struct {
	Repository string       `json:"repository"`
	ObservedAt time.Time    `json:"observedAt"`
	Truncated  bool         `json:"truncated"`
	Flagged    int          `json:"flagged"`
	Counts     map[Role]int `json:"counts"`
	Items      []Item       `json:"items"`
}

type Observer interface {
	Observe(context.Context, string) (Snapshot, error)
}

type AttemptObserver interface {
	ObserveWorker(context.Context, string, string) (WorkerObservation, error)
}

type WorkAttempt struct {
	Sequence  int64      `json:"sequence"`
	ClaimedAt time.Time  `json:"claimedAt"`
	Label     string     `json:"label"`
	Outcome   string     `json:"outcome,omitempty"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

type WorkerObservation struct {
	WorkerID   string       `json:"workerId"`
	Repository string       `json:"repository"`
	ObservedAt time.Time    `json:"observedAt"`
	Status     string       `json:"status"`
	Detail     string       `json:"detail"`
	ItemID     string       `json:"itemId,omitempty"`
	Kind       string       `json:"kind,omitempty"`
	ItemStatus string       `json:"itemStatus,omitempty"`
	Attempt    *WorkAttempt `json:"attempt,omitempty"`
}

type HTTPConfig struct {
	Endpoint   string
	Token      string
	HTTPClient *http.Client
	Now        func() time.Time
}

type HTTPObserver struct {
	endpoint   *url.URL
	token      string
	httpClient *http.Client
	now        func() time.Time
}

func NewHTTPObserver(config HTTPConfig) (*HTTPObserver, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme == "" || endpoint.Host == "" {
		return nil, fmt.Errorf("%w: Snowcat MCP URL must be an absolute HTTP URL", ErrInvalid)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
		return nil, fmt.Errorf("%w: Snowcat MCP URL scheme must be http or https", ErrInvalid)
	}
	if endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, fmt.Errorf("%w: Snowcat MCP URL must not contain user info, a query, or a fragment", ErrInvalid)
	}
	if strings.TrimSpace(config.Token) == "" {
		return nil, fmt.Errorf("%w: Snowcat MCP token is required", ErrInvalid)
	}
	config.Token = strings.TrimSpace(config.Token)
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{
			Timeout: 15 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &HTTPObserver{endpoint: endpoint, token: config.Token, httpClient: config.HTTPClient, now: config.Now}, nil
}

func (observer *HTTPObserver) Observe(ctx context.Context, repository string) (Snapshot, error) {
	if !repositoryRE.MatchString(repository) {
		return Snapshot{}, fmt.Errorf("%w: repository must be owner/name", ErrInvalid)
	}

	remoteItems, err := observer.list(ctx, listArguments{Status: "queued", Repository: repository, Limit: MaxItems})
	if err != nil {
		return Snapshot{}, err
	}
	if len(remoteItems) > MaxItems {
		return Snapshot{}, fmt.Errorf("%w: Snowcat list_work exceeded the requested limit", ErrUnavailable)
	}
	items := make([]Item, 0, len(remoteItems))
	counts := map[Role]int{RoleDiscoverer: 0, RoleImplementer: 0, RoleReviewer: 0, RoleUnassigned: 0}
	flagged := 0
	for _, remote := range remoteItems {
		if remote.Repository != repository || remote.Status != "queued" {
			return Snapshot{}, fmt.Errorf("%w: Snowcat list_work returned an item outside the requested projection", ErrUnavailable)
		}
		role := Classify(remote.Kind)
		contract, contractDetail := assessContract(remote.RequiredArtifact, remote.AllowedActions)
		if role == RoleUnassigned || contract == "ready" {
			counts[role]++
		} else {
			flagged++
		}
		items = append(items, Item{
			ID:               remote.ID,
			Repository:       remote.Repository,
			Kind:             remote.Kind,
			Priority:         remote.Priority,
			AllowedActions:   append([]string(nil), remote.AllowedActions...),
			RequiredArtifact: remote.RequiredArtifact,
			Contract:         contract,
			ContractDetail:   contractDetail,
			Role:             role,
		})
	}
	return Snapshot{
		Repository: repository,
		ObservedAt: observer.now().UTC(),
		Truncated:  len(items) == MaxItems,
		Flagged:    flagged,
		Counts:     counts,
		Items:      items,
	}, nil
}

func (observer *HTTPObserver) ObserveWorker(ctx context.Context, repository, workerID string) (WorkerObservation, error) {
	if !repositoryRE.MatchString(repository) {
		return WorkerObservation{}, fmt.Errorf("%w: repository must be owner/name", ErrInvalid)
	}
	if !workerLabelRE.MatchString(workerID) {
		return WorkerObservation{}, fmt.Errorf("%w: worker ID is invalid", ErrInvalid)
	}
	observedAt := observer.now().UTC()
	remoteItems, err := observer.list(ctx, listArguments{Repository: repository, Label: workerID, Limit: maxCorrelationItems})
	if err != nil {
		return WorkerObservation{}, err
	}
	result := WorkerObservation{
		WorkerID: workerID, Repository: repository, ObservedAt: observedAt,
		Status: "unmatched", Detail: "no Snowcat attempt matched this worker",
	}
	if len(remoteItems) == 0 {
		return result, nil
	}
	matches := make([]*WorkAttempt, len(remoteItems))
	for itemIndex := range remoteItems {
		remote := &remoteItems[itemIndex]
		if remote.Repository != repository || remote.ID == "" || remote.Kind == "" || remote.Status == "" {
			return WorkerObservation{}, fmt.Errorf("%w: Snowcat list_work returned an item outside the requested projection", ErrUnavailable)
		}
		for attemptIndex := range remote.Attempts {
			if remote.Attempts[attemptIndex].Label != workerID {
				continue
			}
			if matches[itemIndex] != nil {
				return WorkerObservation{}, fmt.Errorf("%w: Snowcat did not return exactly one matching attempt", ErrUnavailable)
			}
			matches[itemIndex] = &remote.Attempts[attemptIndex]
		}
		if matches[itemIndex] == nil {
			return WorkerObservation{}, fmt.Errorf("%w: Snowcat did not return exactly one matching attempt", ErrUnavailable)
		}
	}
	if len(remoteItems) > 1 {
		result.Status = "ambiguous"
		result.Detail = "multiple Snowcat items matched this worker; no correlation selected"
		return result, nil
	}
	remote := remoteItems[0]
	matching := matches[0]
	if matching.Sequence < 1 || matching.ClaimedAt.IsZero() {
		return WorkerObservation{}, fmt.Errorf("%w: Snowcat returned an incomplete matching attempt", ErrUnavailable)
	}
	if matching.Outcome != "" && matching.Outcome != "completed" && matching.Outcome != "blocked" && matching.Outcome != "released" && matching.Outcome != "expired" {
		return WorkerObservation{}, fmt.Errorf("%w: Snowcat returned an unknown attempt outcome", ErrUnavailable)
	}
	if matching.Outcome == "" && matching.EndedAt != nil {
		return WorkerObservation{}, fmt.Errorf("%w: Snowcat returned an inconsistent active attempt", ErrUnavailable)
	}
	if matching.Outcome != "" && (matching.EndedAt == nil || matching.EndedAt.Before(matching.ClaimedAt)) {
		return WorkerObservation{}, fmt.Errorf("%w: Snowcat returned an inconsistent terminal attempt", ErrUnavailable)
	}
	result.ItemID = remote.ID
	result.Kind = remote.Kind
	result.ItemStatus = remote.Status
	result.Attempt = matching
	if matching.Outcome == "" {
		result.Status = "claimed"
		result.Detail = "Snowcat reports an active lease for this worker"
	} else {
		result.Status = matching.Outcome
		result.Detail = "Snowcat reports this worker attempt as " + matching.Outcome
	}
	return result, nil
}

func (observer *HTTPObserver) list(ctx context.Context, arguments listArguments) ([]remoteItem, error) {
	requestBody := rpcRequest{JSONRPC: "2.0", ID: 1, Method: "tools/call", Params: rpcParams{Name: "list_work", Arguments: arguments}}
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("encode Snowcat request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, observer.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build Snowcat request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+observer.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	response, err := observer.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: Snowcat MCP request failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64*1024))
		return nil, fmt.Errorf("%w: Snowcat MCP returned HTTP %d", ErrUnavailable, response.StatusCode)
	}
	responsePayload, err := io.ReadAll(io.LimitReader(response.Body, 1024*1024+1))
	if err != nil || len(responsePayload) > 1024*1024 {
		return nil, fmt.Errorf("%w: Snowcat MCP response exceeded the observation limit", ErrUnavailable)
	}
	if strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		responsePayload, err = firstSSEData(responsePayload)
		if err != nil {
			return nil, fmt.Errorf("%w: Snowcat MCP returned invalid event data", ErrUnavailable)
		}
	}
	var envelope rpcResponse
	if err := json.Unmarshal(responsePayload, &envelope); err != nil {
		return nil, fmt.Errorf("%w: Snowcat MCP returned invalid JSON", ErrUnavailable)
	}
	if envelope.Error != nil {
		return nil, fmt.Errorf("%w: Snowcat MCP rejected list_work", ErrUnavailable)
	}
	if envelope.Result.IsError {
		return nil, fmt.Errorf("%w: Snowcat list_work failed", ErrUnavailable)
	}
	if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Type != "text" {
		return nil, fmt.Errorf("%w: Snowcat list_work returned an unexpected result", ErrUnavailable)
	}
	var remoteItems []remoteItem
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &remoteItems); err != nil {
		return nil, fmt.Errorf("%w: Snowcat list_work returned invalid work items", ErrUnavailable)
	}
	if len(remoteItems) > arguments.Limit {
		return nil, fmt.Errorf("%w: Snowcat list_work exceeded the requested limit", ErrUnavailable)
	}
	return remoteItems, nil
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
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
	}
	if len(data) != 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, errors.New("SSE response contained no data event")
}

func assessContract(requiredArtifact string, allowedActions []string) (string, string) {
	if requiredArtifact == "" {
		return "unknown", "Snowcat did not declare requiredArtifact"
	}
	if requiredArtifact != "none" && requiredArtifact != "pull-request" {
		return "unknown", "Snowcat returned an unknown requiredArtifact"
	}
	hasWrite := false
	hasOpenPR := false
	for _, action := range allowedActions {
		hasWrite = hasWrite || action == "write"
		hasOpenPR = hasOpenPR || action == "open-pr"
	}
	if requiredArtifact == "pull-request" && !hasOpenPR {
		return "suspicious", "pull-request delivery lacks open-pr authority"
	}
	if hasWrite && (requiredArtifact != "pull-request" || !hasOpenPR) {
		return "suspicious", "write authority lacks a complete pull-request delivery contract"
	}
	return "ready", ""
}

func Classify(kind string) Role {
	switch {
	case strings.HasSuffix(kind, "-discovery"):
		return RoleDiscoverer
	case kind == "pr-review":
		return RoleReviewer
	case kind == "release-needed":
		return RoleUnassigned
	default:
		return RoleImplementer
	}
}

type rpcRequest struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int       `json:"id"`
	Method  string    `json:"method"`
	Params  rpcParams `json:"params"`
}

type rpcParams struct {
	Name      string        `json:"name"`
	Arguments listArguments `json:"arguments"`
}

type listArguments struct {
	Status     string `json:"status,omitempty"`
	Repository string `json:"repository,omitempty"`
	Label      string `json:"label,omitempty"`
	Limit      int    `json:"limit"`
}

type rpcResponse struct {
	Result rpcResult `json:"result"`
	Error  any       `json:"error"`
}

type rpcResult struct {
	Content []rpcContent `json:"content"`
	IsError bool         `json:"isError"`
}

type rpcContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type remoteItem struct {
	ID               string        `json:"id"`
	Repository       string        `json:"repository"`
	Kind             string        `json:"kind"`
	Priority         int           `json:"priority"`
	Status           string        `json:"status"`
	AllowedActions   []string      `json:"allowedActions"`
	RequiredArtifact string        `json:"requiredArtifact"`
	Attempts         []WorkAttempt `json:"attempts"`
}
