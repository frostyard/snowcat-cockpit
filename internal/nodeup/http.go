package nodeup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
)

const maxResponseBytes = 1 << 20

// HTTPClient drives the running node's loopback API.
type HTTPClient struct {
	BaseURL string
	Client  *http.Client
}

// NewHTTPClient builds a client for the dashboard base URL derived from the
// validated loopback listen address.
func NewHTTPClient(listen string) *HTTPClient {
	return &HTTPClient{BaseURL: "http://" + listen, Client: &http.Client{Timeout: 10 * time.Minute}}
}

// EnrollRepository is idempotent on the node side.
func (client *HTTPClient) EnrollRepository(ctx context.Context, repository string) (managedrepo.Record, error) {
	var record managedrepo.Record
	err := client.call(ctx, http.MethodPost, "/api/v1/repositories", map[string]string{"repository": repository}, &record)
	return record, err
}

// SetupRepository converges one retained managed source.
func (client *HTTPClient) SetupRepository(ctx context.Context, repository string) (managedrepo.Record, error) {
	var record managedrepo.Record
	err := client.call(ctx, http.MethodPost, "/api/v1/repositories/"+repository+"/setup", nil, &record)
	return record, err
}

// Campaign returns the most recent campaign record (zero when none exists).
func (client *HTTPClient) Campaign(ctx context.Context) (campaign.Record, error) {
	var record campaign.Record
	err := client.call(ctx, http.MethodGet, "/api/v1/campaign", nil, &record)
	return record, err
}

// StartCampaign starts one campaign.
func (client *HTTPClient) StartCampaign(ctx context.Context, request campaign.Request) (campaign.Record, error) {
	var record campaign.Record
	err := client.call(ctx, http.MethodPost, "/api/v1/campaign", request, &record)
	return record, err
}

func (client *HTTPClient) call(ctx context.Context, method, path string, body, target any) error {
	if client.BaseURL == "" {
		return errors.New("node client base URL is required")
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.BaseURL+path, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	httpClient := client.Client
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("read %s %s: %w", method, path, err)
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		var failure struct {
			Error string `json:"error"`
		}
		detail := strings.TrimSpace(failure.Error)
		if json.Unmarshal(content, &failure) == nil && failure.Error != "" {
			detail = failure.Error
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, response.StatusCode, detail)
	}
	if target == nil {
		return nil
	}
	if err := json.Unmarshal(content, target); err != nil {
		return fmt.Errorf("decode %s %s: %w", method, path, err)
	}
	return nil
}
