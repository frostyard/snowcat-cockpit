package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frostyard/snowcat-cockpit/internal/preflight"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/state"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

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
	t.Parallel()

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
