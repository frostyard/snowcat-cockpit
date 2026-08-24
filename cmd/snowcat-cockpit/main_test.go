package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/snowcat-cockpit/internal/nodeservice"
	"github.com/frostyard/snowcat-cockpit/internal/preflight"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/state"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

type fakeNodeService struct {
	action         string
	installRequest nodeservice.InstallRequest
	paths          nodeservice.Paths
	result         nodeservice.Result
	uninstall      nodeservice.UninstallResult
	err            error
}

func (service *fakeNodeService) Install(_ context.Context, request nodeservice.InstallRequest) (nodeservice.Result, error) {
	service.action = "install"
	service.installRequest = request
	return service.result, service.err
}

func (service *fakeNodeService) Status(_ context.Context, paths nodeservice.Paths) (nodeservice.Result, error) {
	service.action = "status"
	service.paths = paths
	return service.result, service.err
}

func (service *fakeNodeService) Restart(_ context.Context, paths nodeservice.Paths) (nodeservice.Result, error) {
	service.action = "restart"
	service.paths = paths
	return service.result, service.err
}

func (service *fakeNodeService) Uninstall(_ context.Context, paths nodeservice.Paths) (nodeservice.UninstallResult, error) {
	service.action = "uninstall"
	service.paths = paths
	return service.uninstall, service.err
}

func TestRunNodeInstallProjectsOnlyAllowlistedEnvironment(t *testing.T) {
	service := &fakeNodeService{result: healthyNodeServiceResult()}
	values := map[string]string{
		"PATH":                             "/usr/local/bin:/usr/bin",
		"SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE": "sha256:abc",
		"GH_TOKEN":                         "secret",
		"SNOWCAT_MCP_TOKEN":                "secret",
	}
	lookup := func(name string) (string, bool) {
		value, exists := values[name]
		return value, exists
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runNodeWithService([]string{
		"install", "--listen", "127.0.0.1:7686", "--state-dir", "/state",
		"--skills-dir", "/skills", "--source-root", "/sources",
		"--observer-env", "/config/profile-observer.env",
		"--worker-env", "/config/mcp-token.env",
		"--install-root", "/install", "--unit-dir", "/units",
	}, &stdout, &stderr, service, func() (string, error) { return "/project/dist/snowcat-cockpit", nil }, lookup)
	if code != 0 {
		t.Fatalf("node install exit = %d; stderr = %s", code, stderr.String())
	}
	if service.action != "install" {
		t.Fatalf("action = %q", service.action)
	}
	request := service.installRequest
	if request.Executable != "/project/dist/snowcat-cockpit" || request.Listen != "127.0.0.1:7686" || request.StateDirectory != "/state" || request.SourceRoot != "/sources" || request.InstallRoot != "/install" || request.UnitDirectory != "/units" {
		t.Fatalf("install request = %#v", request)
	}
	if request.ObserverEnv != "/config/profile-observer.env" || request.WorkerEnv != "/config/mcp-token.env" {
		t.Fatalf("credential paths = observer %q, worker %q", request.ObserverEnv, request.WorkerEnv)
	}
	if request.Environment["PATH"] != values["PATH"] || request.Environment["SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE"] != "sha256:abc" {
		t.Fatalf("projected environment = %#v", request.Environment)
	}
	for _, forbidden := range []string{"GH_TOKEN", "SNOWCAT_MCP_TOKEN"} {
		if _, exists := request.Environment[forbidden]; exists {
			t.Fatalf("projected forbidden environment %s", forbidden)
		}
	}
	if !strings.Contains(stdout.String(), "snowcat-cockpit.service active/running") || !strings.Contains(stdout.String(), "http://127.0.0.1:7686") {
		t.Fatalf("node install output = %q", stdout.String())
	}
}

func TestRunNodeLifecycleActionsUseSelectedServicePaths(t *testing.T) {
	tests := []struct {
		action string
	}{
		{action: "status"},
		{action: "restart"},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			service := &fakeNodeService{result: healthyNodeServiceResult()}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			code := runNodeWithService([]string{test.action, "--install-root", "/install", "--unit-dir", "/units", "--json"}, &stdout, &stderr, service, nil, os.LookupEnv)
			if code != 0 {
				t.Fatalf("node %s exit = %d; stderr = %s", test.action, code, stderr.String())
			}
			if service.action != test.action || service.paths.InstallRoot != "/install" || service.paths.UnitDirectory != "/units" {
				t.Fatalf("action = %q, paths = %#v", service.action, service.paths)
			}
			if !strings.Contains(stdout.String(), `"activeState": "active"`) {
				t.Fatalf("JSON output = %q", stdout.String())
			}
		})
	}
}

func TestRunNodeUninstallReportsRetainedState(t *testing.T) {
	service := &fakeNodeService{uninstall: nodeservice.UninstallResult{
		Unit: nodeservice.UnitName, RetainedReleases: 2, StateRetained: true,
	}}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runNodeWithService([]string{"uninstall", "--install-root", "/install", "--unit-dir", "/units"}, &stdout, &stderr, service, nil, os.LookupEnv)
	if code != 0 {
		t.Fatalf("node uninstall exit = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "retained 2 release(s) and all node/worker state") {
		t.Fatalf("uninstall output = %q", stdout.String())
	}
}

func TestRunNodeRejectsNonLoopbackInstallBeforeServiceCall(t *testing.T) {
	service := &fakeNodeService{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runNodeWithService([]string{"install", "--listen", "0.0.0.0:7682"}, &stdout, &stderr, service, func() (string, error) { return "/cockpit", nil }, os.LookupEnv)
	if code != 2 || service.action != "" {
		t.Fatalf("exit = %d, action = %q, stderr = %s", code, service.action, stderr.String())
	}
}

func TestRunNodeHelpDoesNotCallService(t *testing.T) {
	service := &fakeNodeService{}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runNodeWithService([]string{"help"}, &stdout, &stderr, service, nil, os.LookupEnv)
	if code != 0 || service.action != "" || !strings.Contains(stdout.String(), "node install") {
		t.Fatalf("exit = %d, action = %q, stdout = %q, stderr = %q", code, service.action, stdout.String(), stderr.String())
	}
}

func TestRunHelpListsServeSourceRoot(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run([]string{"help"}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "serve [--listen <host:port>] [--state-dir <directory>] [--skills-dir <directory>] [--source-root <directory>]") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
}

func healthyNodeServiceResult() nodeservice.Result {
	health := nodeservice.Health{Status: "ok", NodeID: "node-0123456789abcdef0123456789abcdef", Version: "test-version"}
	return nodeservice.Result{
		Install: nodeservice.Record{
			Unit: nodeservice.UnitName, Release: "test-release", DashboardURL: "http://127.0.0.1:7686",
		},
		Service: nodeservice.ServiceState{LoadState: "loaded", ActiveState: "active", SubState: "running", MainPID: 4242},
		Health:  &health,
	}
}

func TestValidateListenAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		valid   bool
	}{
		{address: "127.0.0.1:7682", valid: true},
		{address: "localhost:443", valid: true},
		{address: "[::1]:7682", valid: true},
		{address: "0.0.0.0:7682", valid: false},
		{address: "192.0.2.10:7682", valid: false},
		{address: "localhost:0", valid: false},
		{address: "localhost", valid: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()
			err := validateListenAddress(test.address)
			if test.valid && err != nil {
				t.Fatalf("expected valid address: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("expected invalid address")
			}
		})
	}
}

func TestWriteWorkerRecordReportsOCIRuntimePosture(t *testing.T) {
	t.Parallel()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	record := worker.Record{
		ID: "worker-0123456789abcdef", Status: worker.StatusRunning,
		Adapter: worker.AdapterOCI, Runtime: worker.RuntimeDocker, RuntimePosture: worker.PostureRootful,
		Workspace: "/tmp/workspace", Branch: "cockpit/worker-0123456789abcdef",
	}
	if code := writeWorkerRecord(&stdout, &stderr, record, false); code != 0 {
		t.Fatalf("writeWorkerRecord exit = %d; stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "(oci, docker rootful)") {
		t.Fatalf("worker output does not report runtime posture: %s", stdout.String())
	}
}

func TestServeRejectsNonLoopbackBeforeCreatingState(t *testing.T) {
	t.Parallel()

	stateDirectory := filepath.Join(t.TempDir(), "must-not-exist")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runServe([]string{
		"--listen", "0.0.0.0:7682",
		"--state-dir", stateDirectory,
	}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %s", code, stderr.String())
	}
	if _, err := os.Stat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("state directory was touched before listen validation: %v", err)
	}
}

func TestQueueObserverConfigurationUsesEnvironmentOnly(t *testing.T) {
	t.Parallel()
	lookup := func(values map[string]string) func(string) string {
		return func(name string) string { return values[name] }
	}

	observer, err := queueObserverFromLookup(lookup(nil))
	if err != nil || observer != nil {
		t.Fatalf("unconfigured observer = %#v, error = %v", observer, err)
	}
	if _, err := queueObserverFromLookup(lookup(map[string]string{"SNOWCAT_COCKPIT_MCP_URL": "https://snowcat.test/mcp"})); err == nil {
		t.Fatal("URL without token was accepted")
	}
	if _, err := queueObserverFromLookup(lookup(map[string]string{"SNOWCAT_COCKPIT_MCP_TOKEN": "secret"})); err == nil {
		t.Fatal("token without URL was accepted")
	}
	observer, err = queueObserverFromLookup(lookup(map[string]string{
		"SNOWCAT_COCKPIT_MCP_URL":   "https://snowcat.test/mcp",
		"SNOWCAT_COCKPIT_MCP_TOKEN": "secret",
	}))
	if err != nil || observer == nil {
		t.Fatalf("configured observer = %#v, error = %v", observer, err)
	}
}

func TestRunInstallKitThenProfiles(t *testing.T) {
	// profiles exits 1 when any provider executable is absent from PATH, so
	// the test supplies all three itself instead of depending on the host.
	binDirectory := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, executable := range []string{"codex", "claude", "copilot"} {
		if err := os.WriteFile(filepath.Join(binDirectory, executable), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDirectory)

	directory := filepath.Join(t.TempDir(), "skills")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := runInstallKit([]string{"--skills-dir", directory}, &stdout, &stderr); code != 0 {
		t.Fatalf("install-kit exit = %d; stderr = %s", code, stderr.String())
	}
	if stdout.String() == "" {
		t.Fatal("install-kit wrote no result")
	}
	stdout.Reset()
	stderr.Reset()
	if code := runProfiles([]string{"--skills-dir", directory, "--state-dir", filepath.Join(t.TempDir(), "state")}, &stdout, &stderr); code != 0 {
		t.Fatalf("profiles exit = %d; stderr = %s", code, stderr.String())
	}
}

type successfulPreflightRunner struct{}

func (successfulPreflightRunner) Run(_ context.Context, _ preflight.Command) ([]byte, error) {
	return []byte("SNOWCAT_COCKPIT_PREFLIGHT_OK skills=work-snowcat-queue,review-snowcat-queue tool=list_work\n"), nil
}

func TestRunPreflightWritesReadyReceipt(t *testing.T) {
	directory := t.TempDir()
	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	providerPath := filepath.Join(binDirectory, "codex")
	if err := os.WriteFile(providerPath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDirectory)
	skillsDirectory := filepath.Join(directory, "skills")
	if _, err := profile.InstallKit(skillsDirectory); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(directory, "state")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := runPreflightWithRunner([]string{
		"--provider", "codex",
		"--mcp-server", "snowcat",
		"--repository", "frostyard/firn",
		"--skills-dir", skillsDirectory,
		"--state-dir", stateDirectory,
	}, &stdout, &stderr, successfulPreflightRunner{})
	if code != 0 {
		t.Fatalf("preflight exit = %d; stderr = %s", code, stderr.String())
	}
	receipts, err := state.ReadPreflights(stateDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if receipts["codex"].Status != "ready" {
		t.Fatalf("receipt = %#v", receipts["codex"])
	}
	entries, err := filepath.Glob(filepath.Join(stateDirectory, ".preflight-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary preflight workspace retained: %#v", entries)
	}
}
