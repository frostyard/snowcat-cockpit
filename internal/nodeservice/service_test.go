package nodeservice

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type runnerCall struct {
	name      string
	arguments []string
}

type fakeRunner struct {
	calls       []runnerCall
	serviceShow string
	errors      map[string]error
}

func (runner *fakeRunner) Run(_ context.Context, name string, arguments ...string) ([]byte, error) {
	call := runnerCall{name: name, arguments: append([]string(nil), arguments...)}
	runner.calls = append(runner.calls, call)
	key := strings.Join(append([]string{name}, arguments...), "\x00")
	if err := runner.errors[key]; err != nil {
		return nil, err
	}
	if containsArgument(arguments, "show") {
		return []byte(runner.serviceShow), nil
	}
	return nil, nil
}

type fakeHealthChecker struct {
	health Health
	err    error
	calls  []string
}

func (checker *fakeHealthChecker) Check(_ context.Context, address string) (Health, error) {
	checker.calls = append(checker.calls, address)
	return checker.health, checker.err
}

type serviceFixture struct {
	manager       *Manager
	runner        *fakeRunner
	health        *fakeHealthChecker
	request       InstallRequest
	paths         Paths
	stateSentinel string
}

func TestInstallPublishesPrivateReleaseAndStartsUserService(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.request.Environment = map[string]string{
		"PATH":                               "/usr/local/bin:/usr/bin",
		"SNOWCAT_COCKPIT_DOCKER_CODEX_IMAGE": "sha256:abc123",
		"GH_TOKEN":                           "must-not-persist",
		"SNOWCAT_COCKPIT_MCP_TOKEN":          "must-not-persist",
	}

	result, err := fixture.manager.Install(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Install.Release == "" || result.Install.BuildVersion != "v1.2.3" || result.Health == nil || result.Health.Version != "v1.2.3" {
		t.Fatalf("install result = %#v", result)
	}
	current, err := os.Readlink(filepath.Join(fixture.request.InstallRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if current != filepath.Join("releases", result.Install.Release) {
		t.Fatalf("current = %q", current)
	}
	assertSameFile(t,
		filepath.Join(fixture.request.InstallRoot, current, "dist", "snowcat-cockpit"),
		fixture.request.Executable,
	)
	assertSameFile(t,
		filepath.Join(fixture.request.InstallRoot, current, "bin", "snowcat-cockpit-serve"),
		fixture.request.Launcher,
	)

	unitPath := filepath.Join(fixture.request.UnitDirectory, UnitName)
	unit := readFile(t, unitPath)
	for _, wanted := range []string{
		"KillMode=process", "Restart=on-failure", "Delegate=yes",
		filepath.Join(fixture.request.InstallRoot, "current", "bin", "snowcat-cockpit-serve"),
		`"--listen" "127.0.0.1:7682"`,
	} {
		if !strings.Contains(unit, wanted) {
			t.Fatalf("unit is missing %q:\n%s", wanted, unit)
		}
	}
	if mode := fileMode(t, unitPath); mode != 0o600 {
		t.Fatalf("unit mode = %o, want 600", mode)
	}
	environment := readFile(t, filepath.Join(fixture.request.InstallRoot, environmentName))
	for _, wanted := range []string{
		`PATH="/usr/local/bin:/usr/bin"`,
		`SNOWCAT_COCKPIT_DOCKER_CODEX_IMAGE="sha256:abc123"`,
		`SNOWCAT_COCKPIT_OBSERVER_ENV="` + fixture.request.ObserverEnv + `"`,
		`SNOWCAT_COCKPIT_WORKER_ENV="` + fixture.request.WorkerEnv + `"`,
	} {
		if !strings.Contains(environment, wanted) {
			t.Fatalf("service environment is missing %q:\n%s", wanted, environment)
		}
	}
	for _, forbidden := range []string{"GH_TOKEN", "SNOWCAT_COCKPIT_MCP_TOKEN", "must-not-persist", "not-read-by-installer"} {
		if strings.Contains(environment, forbidden) || strings.Contains(unit, forbidden) {
			t.Fatalf("generated service persisted forbidden value %q", forbidden)
		}
	}
	wantCalls := [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", UnitName},
		{"--user", "restart", UnitName},
		{"--user", "show", UnitName, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=ExecMainStatus", "--no-pager"},
	}
	if got := runnerArguments(fixture.runner.calls); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("systemctl calls = %#v, want %#v", got, wantCalls)
	}
	for _, call := range fixture.runner.calls {
		if call.name != "systemctl" || len(call.arguments) == 0 || call.arguments[0] != "--user" {
			t.Fatalf("non-user systemctl call: %#v", call)
		}
	}
}

func TestInstallFailsClosedOnMismatchedHealthAndRetainsRelease(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.health.health.Version = "old-version"

	result, err := fixture.manager.Install(context.Background(), fixture.request)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("Install() error = %v, want unhealthy", err)
	}
	if result.Install.Release == "" {
		t.Fatalf("failed install omitted selected release: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(fixture.request.InstallRoot, "releases", result.Install.Release)); err != nil {
		t.Fatalf("failed health check did not retain release: %v", err)
	}
}

func TestRestartUsesInstallRecordAndVerifiesHealth(t *testing.T) {
	fixture := newServiceFixture(t)
	if _, err := fixture.manager.Install(context.Background(), fixture.request); err != nil {
		t.Fatal(err)
	}
	fixture.runner.calls = nil
	fixture.health.calls = nil

	result, err := fixture.manager.Restart(context.Background(), fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if result.Service.ActiveState != "active" || result.Health == nil || len(fixture.health.calls) != 1 {
		t.Fatalf("restart result = %#v, health calls = %q", result, fixture.health.calls)
	}
	want := [][]string{
		{"--user", "restart", UnitName},
		{"--user", "show", UnitName, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=MainPID", "--property=ExecMainStatus", "--no-pager"},
	}
	if got := runnerArguments(fixture.runner.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("restart calls = %#v, want %#v", got, want)
	}
}

func TestStatusReportsInactiveServiceWithoutCallingHealth(t *testing.T) {
	fixture := newServiceFixture(t)
	if _, err := fixture.manager.Install(context.Background(), fixture.request); err != nil {
		t.Fatal(err)
	}
	fixture.runner.calls = nil
	fixture.health.calls = nil
	fixture.runner.serviceShow = serviceProjection("inactive", "dead")

	result, err := fixture.manager.Status(context.Background(), fixture.paths)
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("Status() error = %v, want unhealthy", err)
	}
	if result.Service.ActiveState != "inactive" || len(fixture.health.calls) != 0 {
		t.Fatalf("status result = %#v, health calls = %q", result, fixture.health.calls)
	}
}

func TestUninstallRemovesOnlyServiceSurfaceAndRetainsStateAndReleases(t *testing.T) {
	fixture := newServiceFixture(t)
	result, err := fixture.manager.Install(context.Background(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runner.calls = nil

	uninstalled, err := fixture.manager.Uninstall(context.Background(), fixture.paths)
	if err != nil {
		t.Fatal(err)
	}
	if uninstalled.RetainedReleases != 1 || !uninstalled.StateRetained {
		t.Fatalf("uninstall result = %#v", uninstalled)
	}
	for _, removed := range []string{
		filepath.Join(fixture.request.UnitDirectory, UnitName),
		filepath.Join(fixture.request.InstallRoot, "current"),
		filepath.Join(fixture.request.InstallRoot, environmentName),
		filepath.Join(fixture.request.InstallRoot, recordName),
	} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact %s remains: %v", removed, err)
		}
	}
	if _, err := os.Stat(filepath.Join(fixture.request.InstallRoot, "releases", result.Install.Release)); err != nil {
		t.Fatalf("release was removed: %v", err)
	}
	if content := readFile(t, fixture.stateSentinel); content != "retained\n" {
		t.Fatalf("state sentinel changed: %q", content)
	}
	want := [][]string{
		{"--user", "disable", "--now", UnitName},
		{"--user", "daemon-reload"},
	}
	if got := runnerArguments(fixture.runner.calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("uninstall calls = %#v, want %#v", got, want)
	}
}

func TestInstallRejectsUnsafeInputsBeforeChangingServiceState(t *testing.T) {
	t.Run("non-loopback", func(t *testing.T) {
		fixture := newServiceFixture(t)
		fixture.request.Listen = "0.0.0.0:7682"
		if _, err := fixture.manager.Install(context.Background(), fixture.request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Install() error = %v, want invalid", err)
		}
		if len(fixture.runner.calls) != 0 {
			t.Fatalf("unsafe install reached systemd: %#v", fixture.runner.calls)
		}
	})

	t.Run("credential symlink", func(t *testing.T) {
		fixture := newServiceFixture(t)
		target := fixture.request.ObserverEnv
		link := target + ".link"
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		fixture.request.ObserverEnv = link
		if _, err := fixture.manager.Install(context.Background(), fixture.request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Install() error = %v, want invalid", err)
		}
		if len(fixture.runner.calls) != 0 {
			t.Fatalf("unsafe install reached systemd: %#v", fixture.runner.calls)
		}
	})

	t.Run("worker credential symlink", func(t *testing.T) {
		fixture := newServiceFixture(t)
		target := fixture.request.WorkerEnv
		link := target + ".link"
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		fixture.request.WorkerEnv = link
		if _, err := fixture.manager.Install(context.Background(), fixture.request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Install() error = %v, want invalid", err)
		}
		if len(fixture.runner.calls) != 0 {
			t.Fatalf("unsafe install reached systemd: %#v", fixture.runner.calls)
		}
	})

	t.Run("environment control character", func(t *testing.T) {
		fixture := newServiceFixture(t)
		fixture.request.Environment = map[string]string{"PATH": "/usr/bin\nEnvironment=TOKEN=bad"}
		if _, err := fixture.manager.Install(context.Background(), fixture.request); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Install() error = %v, want invalid", err)
		}
		if len(fixture.runner.calls) != 0 {
			t.Fatalf("unsafe install reached systemd: %#v", fixture.runner.calls)
		}
	})
}

func TestManagerRefusesSystemdOperationsOutsideLinux(t *testing.T) {
	runner := &fakeRunner{}
	health := &fakeHealthChecker{}
	manager, err := New(Config{Runner: runner, Health: health, GOOS: "darwin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Status(context.Background(), Paths{InstallRoot: "/tmp/install", UnitDirectory: "/tmp/units"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Status() error = %v, want unavailable", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("non-Linux status reached systemd: %#v", runner.calls)
	}
}

func newServiceFixture(t *testing.T) serviceFixture {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(filepath.Join(project, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(project, "dist", "snowcat-cockpit")
	launcher := filepath.Join(project, "bin", "snowcat-cockpit-serve")
	if err := os.WriteFile(executable, []byte("cockpit-v1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/sh\nexec cockpit\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	observerEnv := filepath.Join(root, "profile-observer.env")
	if err := os.WriteFile(observerEnv, []byte("export SNOWCAT_OBSERVER_TOKEN=not-read-by-installer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerEnv := filepath.Join(root, "mcp-token.env")
	if err := os.WriteFile(workerEnv, []byte("export SNOWCAT_MCP_TOKEN=not-read-by-installer\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDirectory := filepath.Join(root, "state")
	if err := os.Mkdir(stateDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	stateSentinel := filepath.Join(stateDirectory, "sentinel")
	if err := os.WriteFile(stateSentinel, []byte("retained\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{serviceShow: serviceProjection("active", "running"), errors: map[string]error{}}
	health := &fakeHealthChecker{health: Health{
		Status: "ok", NodeID: "node-0123456789abcdef0123456789abcdef",
		StartedAt: time.Date(2026, 8, 23, 23, 0, 0, 0, time.UTC), Version: "v1.2.3",
	}}
	manager, err := New(Config{
		Runner: runner, Health: health, GOOS: "linux", Attempts: 1,
		Now: func() time.Time { return time.Date(2026, 8, 23, 22, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := InstallRequest{
		Executable: executable, Launcher: launcher, Version: "v1.2.3", Listen: "127.0.0.1:7682",
		StateDirectory: stateDirectory, SkillsDirectory: filepath.Join(root, "skills"),
		SourceRoot: filepath.Join(root, "sources"), ObserverEnv: observerEnv, WorkerEnv: workerEnv,
		InstallRoot: filepath.Join(root, "install"), UnitDirectory: filepath.Join(root, "config", "systemd", "user"),
	}
	return serviceFixture{
		manager: manager, runner: runner, health: health, request: request,
		paths:         Paths{InstallRoot: request.InstallRoot, UnitDirectory: request.UnitDirectory},
		stateSentinel: stateSentinel,
	}
}

func serviceProjection(active, sub string) string {
	return "LoadState=loaded\nActiveState=" + active + "\nSubState=" + sub + "\nMainPID=4242\nExecMainStatus=0\n"
}

func runnerArguments(calls []runnerCall) [][]string {
	result := make([][]string, 0, len(calls))
	for _, call := range calls {
		result = append(result, call.arguments)
	}
	return result
}

func containsArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
}

func assertSameFile(t *testing.T, left, right string) {
	t.Helper()
	leftContent := readFile(t, left)
	rightContent := readFile(t, right)
	if leftContent != rightContent {
		t.Fatalf("files differ:\n%s\n%s", left, right)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
