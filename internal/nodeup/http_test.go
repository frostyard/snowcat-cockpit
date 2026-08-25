package nodeup

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
)

func TestHTTPClientDrivesTheNodeAPI(t *testing.T) {
	t.Parallel()

	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		calls = append(calls, request.Method+" "+request.URL.Path)
		response.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "POST /api/v1/repositories":
			var body map[string]string
			_ = json.NewDecoder(request.Body).Decode(&body)
			response.WriteHeader(http.StatusCreated)
			_, _ = response.Write([]byte(`{"repository":"` + body["repository"] + `","status":"pending"}`))
		case "POST /api/v1/repositories/frostyard/clix/setup":
			_, _ = response.Write([]byte(`{"repository":"frostyard/clix","status":"ready","baseCommit":"abc"}`))
		case "GET /api/v1/campaign":
			_, _ = response.Write([]byte(`{"status":"stopped","id":"campaign-old"}`))
		case "POST /api/v1/campaign":
			var body campaign.Request
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body.Reviewer.MCPServer != "snowcat-mcp" {
				response.WriteHeader(http.StatusBadRequest)
				_, _ = response.Write([]byte(`{"error":"invalid board campaign: reviewer"}`))
				return
			}
			response.WriteHeader(http.StatusAccepted)
			_, _ = response.Write([]byte(`{"status":"starting","id":"campaign-new"}`))
		default:
			response.WriteHeader(http.StatusNotFound)
			_, _ = response.Write([]byte(`{"error":"managed repository not found"}`))
		}
	}))
	defer server.Close()

	client := &HTTPClient{BaseURL: server.URL, Client: server.Client()}
	ctx := context.Background()
	if record, err := client.EnrollRepository(ctx, "frostyard/clix"); err != nil || record.Repository != "frostyard/clix" {
		t.Fatalf("enroll = %#v, %v", record, err)
	}
	if record, err := client.SetupRepository(ctx, "frostyard/clix"); err != nil || record.Status != "ready" || record.BaseCommit != "abc" {
		t.Fatalf("setup = %#v, %v", record, err)
	}
	if record, err := client.Campaign(ctx); err != nil || record.ID != "campaign-old" || record.Status != campaign.StatusStopped {
		t.Fatalf("campaign = %#v, %v", record, err)
	}
	if record, err := client.StartCampaign(ctx, campaign.Request{Reviewer: campaign.Lane{MCPServer: "snowcat-mcp"}}); err != nil || record.ID != "campaign-new" {
		t.Fatalf("start = %#v, %v", record, err)
	}
	if _, err := client.StartCampaign(ctx, campaign.Request{}); err == nil || !strings.Contains(err.Error(), "HTTP 400") || !strings.Contains(err.Error(), "invalid board campaign") {
		t.Fatalf("start error = %v", err)
	}
	if _, err := client.SetupRepository(ctx, "frostyard/missing"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("setup error = %v", err)
	}
	if len(calls) != 6 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestNewHTTPClientTargetsLoopbackListen(t *testing.T) {
	t.Parallel()

	client := NewHTTPClient("127.0.0.1:7686")
	if client.BaseURL != "http://127.0.0.1:7686" || client.Client == nil {
		t.Fatalf("client = %#v", client)
	}
	if err := (&HTTPClient{}).call(context.Background(), http.MethodGet, "/", nil, nil); err == nil {
		t.Fatal("empty base URL must fail")
	}
}
