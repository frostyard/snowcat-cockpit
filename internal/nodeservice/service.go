package nodeservice

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	UnitName        = "snowcat-cockpit.service"
	recordName      = "install.json"
	environmentName = "service.env"
	recordVersion   = 1
)

var releasePartRE = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

var EnvironmentAllowlist = []string{
	"PATH",
	"XDG_CONFIG_HOME",
	"XDG_RUNTIME_DIR",
	"CODEX_HOME",
	"CLAUDE_CONFIG_DIR",
	"COPILOT_HOME",
	"GH_CONFIG_DIR",
	"SNOWCAT_COCKPIT_OCI_IMAGE",
	"SNOWCAT_COCKPIT_OCI_CODEX_IMAGE",
	"SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE",
	"SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE",
	"SNOWCAT_COCKPIT_DOCKER_CODEX_IMAGE",
	"SNOWCAT_COCKPIT_DOCKER_CLAUDE_IMAGE",
	"SNOWCAT_COCKPIT_DOCKER_COPILOT_IMAGE",
	"SNOWCAT_COCKPIT_DOCKER_ADD_HOST",
}

var (
	ErrInvalid     = errors.New("invalid node service")
	ErrUnavailable = errors.New("node service unavailable")
	ErrUnhealthy   = errors.New("node service unhealthy")
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type OSRunner struct{}

func (OSRunner) Run(ctx context.Context, name string, arguments ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, arguments...).CombinedOutput()
}

type HealthChecker interface {
	Check(context.Context, string) (Health, error)
}

type HTTPHealthChecker struct {
	Client *http.Client
}

func (checker HTTPHealthChecker) Check(ctx context.Context, address string) (Health, error) {
	client := checker.Client
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, dashboardURL(address)+"/api/v1/health", nil)
	if err != nil {
		return Health{}, fmt.Errorf("build node health request: %w", err)
	}
	response, err := client.Do(request)
	if err != nil {
		return Health{}, fmt.Errorf("query node health: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("query node health: HTTP %d", response.StatusCode)
	}
	var health Health
	decoder := json.NewDecoder(io.LimitReader(response.Body, 32*1024))
	if err := decoder.Decode(&health); err != nil {
		return Health{}, fmt.Errorf("decode node health: %w", err)
	}
	if health.Status != "ok" || health.NodeID == "" || health.Version == "" {
		return Health{}, fmt.Errorf("%w: node health projection is incomplete", ErrUnhealthy)
	}
	return health, nil
}

type Config struct {
	Runner     Runner
	Health     HealthChecker
	GOOS       string
	Now        func() time.Time
	Attempts   int
	RetryDelay time.Duration
	Sleep      func(context.Context, time.Duration) error
}

type Manager struct {
	runner     Runner
	health     HealthChecker
	goos       string
	now        func() time.Time
	attempts   int
	retryDelay time.Duration
	sleep      func(context.Context, time.Duration) error
}

func New(config Config) (*Manager, error) {
	if config.Runner == nil || config.Health == nil || config.GOOS == "" {
		return nil, fmt.Errorf("%w: runner, health checker, and platform are required", ErrInvalid)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Attempts == 0 {
		config.Attempts = 20
	}
	if config.Attempts < 1 {
		return nil, fmt.Errorf("%w: health attempts must be positive", ErrInvalid)
	}
	if config.RetryDelay == 0 {
		config.RetryDelay = 250 * time.Millisecond
	}
	if config.Sleep == nil {
		config.Sleep = sleepContext
	}
	return &Manager{
		runner: config.Runner, health: config.Health, goos: config.GOOS,
		now: config.Now, attempts: config.Attempts, retryDelay: config.RetryDelay, sleep: config.Sleep,
	}, nil
}

type InstallRequest struct {
	Executable      string
	Launcher        string
	Version         string
	Listen          string
	StateDirectory  string
	SkillsDirectory string
	SourceRoot      string
	ObserverEnv     string
	WorkerEnv       string
	InstallRoot     string
	UnitDirectory   string
	ConfigPath      string
	Environment     map[string]string
}

type Paths struct {
	InstallRoot   string
	UnitDirectory string
}

type Record struct {
	Version         int       `json:"version"`
	Unit            string    `json:"unit"`
	Release         string    `json:"release"`
	BuildVersion    string    `json:"buildVersion"`
	Listen          string    `json:"listen"`
	DashboardURL    string    `json:"dashboardUrl"`
	StateDirectory  string    `json:"stateDirectory"`
	SkillsDirectory string    `json:"skillsDirectory"`
	SourceRoot      string    `json:"sourceRoot"`
	InstallRoot     string    `json:"installRoot"`
	UnitPath        string    `json:"unitPath"`
	EnvironmentPath string    `json:"environmentPath"`
	ConfigPath      string    `json:"configPath,omitempty"`
	InstalledAt     time.Time `json:"installedAt"`
}

// InstallPlan is the derived shape an install request converges to: the
// content-addressed release ID and the exact service environment file the
// install would write. It lets a caller decide whether an install is already
// in place without changing service state.
type InstallPlan struct {
	Release         string
	Listen          string
	StateDirectory  string
	SkillsDirectory string
	SourceRoot      string
	Environment     string
}

type ServiceState struct {
	LoadState      string `json:"loadState"`
	ActiveState    string `json:"activeState"`
	SubState       string `json:"subState"`
	MainPID        int    `json:"mainPid"`
	ExecMainStatus int    `json:"execMainStatus"`
}

type Health struct {
	Status    string    `json:"status"`
	NodeID    string    `json:"nodeId"`
	StartedAt time.Time `json:"startedAt"`
	Version   string    `json:"version"`
}

type Result struct {
	Install Record       `json:"install"`
	Service ServiceState `json:"service"`
	Health  *Health      `json:"health,omitempty"`
}

type UninstallResult struct {
	Unit             string `json:"unit"`
	UnitPath         string `json:"unitPath"`
	RetainedReleases int    `json:"retainedReleases"`
	StateRetained    bool   `json:"stateRetained"`
}

func (manager *Manager) Install(ctx context.Context, request InstallRequest) (Result, error) {
	if err := manager.requireLinux(); err != nil {
		return Result{}, err
	}
	normalized, executable, launcher, err := normalizeInstallRequest(request)
	if err != nil {
		return Result{}, err
	}
	if err := validateCredentialEnv(normalized.ObserverEnv, "observer"); err != nil {
		return Result{}, err
	}
	if err := validateCredentialEnv(normalized.WorkerEnv, "worker"); err != nil {
		return Result{}, err
	}
	releaseHash, err := releaseContentHash(executable, launcher)
	if err != nil {
		return Result{}, err
	}
	release := releaseID(normalized.Version, releaseHash)
	releaseDirectory := filepath.Join(normalized.InstallRoot, "releases", release)
	if err := installRelease(releaseDirectory, executable, launcher); err != nil {
		return Result{}, err
	}

	environmentPath := filepath.Join(normalized.InstallRoot, environmentName)
	environment, err := renderEnvironment(normalized.ObserverEnv, normalized.WorkerEnv, normalized.Environment)
	if err != nil {
		return Result{}, err
	}
	if err := writeAtomic(environmentPath, []byte(environment), 0o600); err != nil {
		return Result{}, fmt.Errorf("write node service environment: %w", err)
	}
	unitPath := filepath.Join(normalized.UnitDirectory, UnitName)
	unit, err := renderUnit(normalized, environmentPath)
	if err != nil {
		return Result{}, err
	}
	if err := writeAtomic(unitPath, []byte(unit), 0o600); err != nil {
		return Result{}, fmt.Errorf("write node service unit: %w", err)
	}
	if err := switchCurrent(normalized.InstallRoot, release); err != nil {
		return Result{}, err
	}
	record := Record{
		Version: recordVersion, Unit: UnitName, Release: release, BuildVersion: normalized.Version,
		Listen: normalized.Listen, DashboardURL: dashboardURL(normalized.Listen),
		StateDirectory: normalized.StateDirectory, SkillsDirectory: normalized.SkillsDirectory,
		SourceRoot: normalized.SourceRoot, InstallRoot: normalized.InstallRoot,
		UnitPath: unitPath, EnvironmentPath: environmentPath, ConfigPath: normalized.ConfigPath,
		InstalledAt: manager.now().UTC(),
	}
	if err := writeJSONAtomic(filepath.Join(normalized.InstallRoot, recordName), record); err != nil {
		return Result{}, err
	}
	if _, err := manager.systemctl(ctx, "daemon-reload"); err != nil {
		return Result{Install: record}, fmt.Errorf("reload node service manager: %w", err)
	}
	if _, err := manager.systemctl(ctx, "enable", UnitName); err != nil {
		return Result{Install: record}, fmt.Errorf("enable node service: %w", err)
	}
	if _, err := manager.systemctl(ctx, "restart", UnitName); err != nil {
		return Result{Install: record}, fmt.Errorf("restart node service: %w", err)
	}
	return manager.verify(ctx, record)
}

// PlanInstall validates the request exactly as Install does and returns the
// release ID and rendered service environment without touching the release
// root, the unit, or systemd.
func PlanInstall(request InstallRequest) (InstallPlan, error) {
	normalized, executable, launcher, err := normalizeInstallRequest(request)
	if err != nil {
		return InstallPlan{}, err
	}
	releaseHash, err := releaseContentHash(executable, launcher)
	if err != nil {
		return InstallPlan{}, err
	}
	environment, err := renderEnvironment(normalized.ObserverEnv, normalized.WorkerEnv, normalized.Environment)
	if err != nil {
		return InstallPlan{}, err
	}
	return InstallPlan{
		Release: releaseID(normalized.Version, releaseHash), Listen: normalized.Listen,
		StateDirectory: normalized.StateDirectory, SkillsDirectory: normalized.SkillsDirectory,
		SourceRoot: normalized.SourceRoot, Environment: environment,
	}, nil
}

// ValidateListen reports whether address is a loopback host:port.
func ValidateListen(address string) error {
	return validateListenAddress(address)
}

func (manager *Manager) Status(ctx context.Context, paths Paths) (Result, error) {
	if err := manager.requireLinux(); err != nil {
		return Result{}, err
	}
	record, err := readRecord(paths)
	if err != nil {
		return Result{}, err
	}
	service, err := manager.readServiceState(ctx)
	result := Result{Install: record, Service: service}
	if err != nil {
		return result, err
	}
	if service.ActiveState != "active" {
		return result, fmt.Errorf("%w: service is %s/%s", ErrUnhealthy, service.ActiveState, service.SubState)
	}
	health, err := manager.health.Check(ctx, record.Listen)
	if err != nil {
		return result, err
	}
	result.Health = &health
	if health.Version != record.BuildVersion {
		return result, fmt.Errorf("%w: health reports version %s, want %s", ErrUnhealthy, health.Version, record.BuildVersion)
	}
	return result, nil
}

func (manager *Manager) Restart(ctx context.Context, paths Paths) (Result, error) {
	if err := manager.requireLinux(); err != nil {
		return Result{}, err
	}
	record, err := readRecord(paths)
	if err != nil {
		return Result{}, err
	}
	if _, err := manager.systemctl(ctx, "restart", UnitName); err != nil {
		return Result{Install: record}, fmt.Errorf("restart node service: %w", err)
	}
	return manager.verify(ctx, record)
}

func (manager *Manager) Uninstall(ctx context.Context, paths Paths) (UninstallResult, error) {
	if err := manager.requireLinux(); err != nil {
		return UninstallResult{}, err
	}
	installRoot, unitDirectory, err := normalizePaths(paths)
	if err != nil {
		return UninstallResult{}, err
	}
	unitPath := filepath.Join(unitDirectory, UnitName)
	if _, err := manager.systemctl(ctx, "disable", "--now", UnitName); err != nil {
		return UninstallResult{}, fmt.Errorf("disable node service: %w", err)
	}
	for _, target := range []struct {
		path      string
		allowLink bool
	}{
		{unitPath, false},
		{filepath.Join(installRoot, "current"), true},
		{filepath.Join(installRoot, environmentName), false},
		{filepath.Join(installRoot, recordName), false},
	} {
		if err := removeExact(target.path, target.allowLink); err != nil {
			return UninstallResult{}, err
		}
	}
	if _, err := manager.systemctl(ctx, "daemon-reload"); err != nil {
		return UninstallResult{}, fmt.Errorf("reload node service manager: %w", err)
	}
	return UninstallResult{
		Unit: UnitName, UnitPath: unitPath,
		RetainedReleases: countReleases(filepath.Join(installRoot, "releases")), StateRetained: true,
	}, nil
}

func (manager *Manager) verify(ctx context.Context, record Record) (Result, error) {
	var result Result
	result.Install = record
	var last error
	for attempt := 0; attempt < manager.attempts; attempt++ {
		service, err := manager.readServiceState(ctx)
		result.Service = service
		if err == nil && service.ActiveState == "active" {
			health, healthErr := manager.health.Check(ctx, record.Listen)
			if healthErr == nil {
				result.Health = &health
				if health.Version == record.BuildVersion {
					return result, nil
				}
				healthErr = fmt.Errorf("health reports version %s, want %s", health.Version, record.BuildVersion)
			}
			last = healthErr
		} else if err != nil {
			last = err
		} else {
			last = fmt.Errorf("service is %s/%s", service.ActiveState, service.SubState)
		}
		if attempt+1 < manager.attempts {
			if err := manager.sleep(ctx, manager.retryDelay); err != nil {
				return result, err
			}
		}
	}
	return result, fmt.Errorf("%w: %v", ErrUnhealthy, last)
}

func (manager *Manager) readServiceState(ctx context.Context) (ServiceState, error) {
	output, err := manager.systemctl(ctx, "show", UnitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState",
		"--property=MainPID", "--property=ExecMainStatus", "--no-pager")
	if err != nil {
		return ServiceState{}, fmt.Errorf("read node service state: %w", err)
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		name, value, found := strings.Cut(line, "=")
		if found {
			values[name] = value
		}
	}
	mainPID, pidErr := strconv.Atoi(values["MainPID"])
	exitStatus, statusErr := strconv.Atoi(values["ExecMainStatus"])
	if values["LoadState"] == "" || values["ActiveState"] == "" || values["SubState"] == "" || pidErr != nil || statusErr != nil {
		return ServiceState{}, fmt.Errorf("%w: systemd returned an incomplete service projection", ErrUnavailable)
	}
	return ServiceState{
		LoadState: values["LoadState"], ActiveState: values["ActiveState"], SubState: values["SubState"],
		MainPID: mainPID, ExecMainStatus: exitStatus,
	}, nil
}

func (manager *Manager) systemctl(ctx context.Context, arguments ...string) ([]byte, error) {
	return manager.runner.Run(ctx, "systemctl", append([]string{"--user"}, arguments...)...)
}

func (manager *Manager) requireLinux() error {
	if manager.goos != "linux" {
		return fmt.Errorf("%w: systemd user services require Linux", ErrUnavailable)
	}
	return nil
}

func normalizeInstallRequest(request InstallRequest) (InstallRequest, string, string, error) {
	if request.Executable == "" || request.Version == "" || request.Listen == "" || request.StateDirectory == "" || request.SkillsDirectory == "" || request.ObserverEnv == "" || request.WorkerEnv == "" || request.InstallRoot == "" || request.UnitDirectory == "" {
		return InstallRequest{}, "", "", fmt.Errorf("%w: executable, version, listen, state, skills, observer environment, worker environment, install root, and unit directory are required", ErrInvalid)
	}
	if err := validateListenAddress(request.Listen); err != nil {
		return InstallRequest{}, "", "", err
	}
	var err error
	for _, target := range []struct {
		value *string
		name  string
	}{
		{&request.StateDirectory, "state directory"}, {&request.SkillsDirectory, "skills directory"},
		{&request.SourceRoot, "source root"}, {&request.ObserverEnv, "observer environment"},
		{&request.WorkerEnv, "worker environment"},
		{&request.InstallRoot, "install root"}, {&request.UnitDirectory, "unit directory"},
	} {
		if *target.value == "" && target.name == "source root" {
			*target.value = filepath.Join(request.StateDirectory, "sources")
		}
		*target.value, err = absoluteClean(*target.value)
		if err != nil {
			return InstallRequest{}, "", "", fmt.Errorf("%w: resolve %s", ErrInvalid, target.name)
		}
	}
	if request.ConfigPath != "" {
		request.ConfigPath, err = absoluteClean(request.ConfigPath)
		if err != nil {
			return InstallRequest{}, "", "", fmt.Errorf("%w: resolve config path", ErrInvalid)
		}
	}
	executable, err := executableFile(request.Executable)
	if err != nil {
		return InstallRequest{}, "", "", fmt.Errorf("%w: executable: %v", ErrInvalid, err)
	}
	launcher := request.Launcher
	if launcher == "" {
		launcher = filepath.Join(filepath.Dir(filepath.Dir(executable)), "bin", "snowcat-cockpit-serve")
	}
	launcher, err = executableFile(launcher)
	if err != nil {
		return InstallRequest{}, "", "", fmt.Errorf("%w: companion launcher: %v", ErrInvalid, err)
	}
	return request, executable, launcher, nil
}

func normalizePaths(paths Paths) (string, string, error) {
	if paths.InstallRoot == "" || paths.UnitDirectory == "" {
		return "", "", fmt.Errorf("%w: install root and unit directory are required", ErrInvalid)
	}
	installRoot, err := absoluteClean(paths.InstallRoot)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve install root", ErrInvalid)
	}
	unitDirectory, err := absoluteClean(paths.UnitDirectory)
	if err != nil {
		return "", "", fmt.Errorf("%w: resolve unit directory", ErrInvalid)
	}
	return installRoot, unitDirectory, nil
}

func readRecord(paths Paths) (Record, error) {
	installRoot, _, err := normalizePaths(paths)
	if err != nil {
		return Record{}, err
	}
	content, err := os.ReadFile(filepath.Join(installRoot, recordName))
	if err != nil {
		return Record{}, fmt.Errorf("read node service install record: %w", err)
	}
	var record Record
	if err := json.Unmarshal(content, &record); err != nil {
		return Record{}, fmt.Errorf("decode node service install record: %w", err)
	}
	if record.Version != recordVersion || record.Unit != UnitName || record.InstallRoot != installRoot || record.BuildVersion == "" || record.Listen == "" {
		return Record{}, fmt.Errorf("%w: install record does not match this service", ErrInvalid)
	}
	return record, nil
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: listen address must be host:port", ErrInvalid)
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("%w: listen host must be loopback", ErrInvalid)
		}
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("%w: listen port must be between 1 and 65535", ErrInvalid)
	}
	return nil
}

func validateCredentialEnv(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspect %s credential file: %v", ErrInvalid, label, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s credential file must be a regular non-symlink file", ErrInvalid, label)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%w: %s credential file must have mode 0600", ErrInvalid, label)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%w: %s credential file must be owned by the current user", ErrInvalid, label)
	}
	return nil
}

func executableFile(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = absoluteClean(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("must be an executable regular file")
	}
	return resolved, nil
}

func releaseContentHash(paths ...string) (string, error) {
	hash := sha256.New()
	for _, path := range paths {
		if _, err := io.WriteString(hash, filepath.Base(path)+"\x00"); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("hash node service release: %w", err)
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash node service release: %w", copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("hash node service release: %w", closeErr)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func releaseID(version, hash string) string {
	part := strings.Trim(releasePartRE.ReplaceAllString(version, "-"), "-._")
	if part == "" {
		part = "development"
	}
	return part + "-" + hash[:16]
}

func installRelease(destination, executable, launcher string) error {
	releases := filepath.Dir(destination)
	if err := os.MkdirAll(releases, 0o700); err != nil {
		return fmt.Errorf("create node service release root: %w", err)
	}
	if info, err := os.Lstat(destination); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: release destination is not a real directory", ErrInvalid)
		}
		matches, err := releaseMatches(destination, executable, launcher)
		if err != nil {
			return err
		}
		if !matches {
			return fmt.Errorf("%w: existing content-addressed release does not match", ErrInvalid)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect node service release: %w", err)
	}
	temporary, err := os.MkdirTemp(releases, ".release-")
	if err != nil {
		return fmt.Errorf("create node service release: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := os.Chmod(temporary, 0o700); err != nil {
		return fmt.Errorf("secure node service release: %w", err)
	}
	for _, directory := range []string{filepath.Join(temporary, "bin"), filepath.Join(temporary, "dist")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return fmt.Errorf("create node service release layout: %w", err)
		}
	}
	if err := copyFile(filepath.Join(temporary, "dist", "snowcat-cockpit"), executable, 0o755); err != nil {
		return err
	}
	if err := copyFile(filepath.Join(temporary, "bin", "snowcat-cockpit-serve"), launcher, 0o755); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return fmt.Errorf("publish node service release: %w", err)
	}
	return nil
}

func releaseMatches(destination, executable, launcher string) (bool, error) {
	wanted, err := releaseContentHash(executable, launcher)
	if err != nil {
		return false, err
	}
	installed, err := releaseContentHash(
		filepath.Join(destination, "dist", "snowcat-cockpit"),
		filepath.Join(destination, "bin", "snowcat-cockpit-serve"),
	)
	if err != nil {
		return false, err
	}
	return installed == wanted, nil
}

func copyFile(destination, source string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open node service release input: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("create node service release file: %w", err)
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return fmt.Errorf("copy node service release file: %w", copyErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync node service release file: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close node service release file: %w", closeErr)
	}
	return nil
}

func renderEnvironment(observerEnv, workerEnv string, environment map[string]string) (string, error) {
	values := map[string]string{
		"SNOWCAT_COCKPIT_OBSERVER_ENV": observerEnv,
		"SNOWCAT_COCKPIT_WORKER_ENV":   workerEnv,
	}
	for _, name := range EnvironmentAllowlist {
		if value := environment[name]; value != "" {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	var output strings.Builder
	for _, name := range names {
		value, err := quoteEnvironmentValue(values[name])
		if err != nil {
			return "", fmt.Errorf("%w: environment %s", ErrInvalid, name)
		}
		fmt.Fprintf(&output, "%s=%s\n", name, value)
	}
	return output.String(), nil
}

func renderUnit(request InstallRequest, environmentPath string) (string, error) {
	executable := filepath.Join(request.InstallRoot, "current", "bin", "snowcat-cockpit-serve")
	arguments := []string{
		executable, "--listen", request.Listen, "--state-dir", request.StateDirectory,
		"--skills-dir", request.SkillsDirectory, "--source-root", request.SourceRoot,
	}
	quoted := make([]string, 0, len(arguments))
	for _, argument := range arguments {
		value, err := quoteUnitValue(argument)
		if err != nil {
			return "", fmt.Errorf("%w: service argument", ErrInvalid)
		}
		quoted = append(quoted, value)
	}
	unitEnvironment, err := environmentFileDirectivePath(environmentPath)
	if err != nil {
		return "", fmt.Errorf("%w: service environment path", ErrInvalid)
	}
	return fmt.Sprintf(`[Unit]
Description=Snowcat Cockpit node
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=%s
ExecStart=%s
Restart=on-failure
RestartSec=5s
KillMode=process
TimeoutStopSec=15s
UMask=0077
Delegate=yes

[Install]
WantedBy=default.target
`, unitEnvironment, strings.Join(quoted, " ")), nil
}

func environmentFileDirectivePath(value string) (string, error) {
	if err := rejectControls(value); err != nil {
		return "", err
	}
	if !filepath.IsAbs(value) {
		return "", errors.New("must be absolute")
	}
	if strings.ContainsAny(value, `\"'*?[]`) {
		return "", errors.New("contains unsupported systemd path syntax")
	}
	for _, character := range value {
		if unicode.IsSpace(character) {
			return "", errors.New("contains unsupported whitespace")
		}
	}
	return strings.ReplaceAll(value, "%", "%%"), nil
}

func quoteEnvironmentValue(value string) (string, error) {
	if err := rejectControls(value); err != nil {
		return "", err
	}
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `"` + replacer.Replace(value) + `"`, nil
}

func quoteUnitValue(value string) (string, error) {
	if err := rejectControls(value); err != nil {
		return "", err
	}
	value = strings.ReplaceAll(value, "%", "%%")
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + replacer.Replace(value) + `"`, nil
}

func rejectControls(value string) error {
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return errors.New("control characters are forbidden")
		}
	}
	return nil
}

func switchCurrent(root, release string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("create node service install root: %w", err)
	}
	temporary := filepath.Join(root, ".current-"+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Symlink(filepath.Join("releases", release), temporary); err != nil {
		return fmt.Errorf("create node service release selection: %w", err)
	}
	defer func() { _ = os.Remove(temporary) }()
	current := filepath.Join(root, "current")
	if info, err := os.Lstat(current); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return fmt.Errorf("%w: current release selection is not a symlink", ErrInvalid)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect node service release selection: %w", err)
	}
	if err := os.Rename(temporary, current); err != nil {
		return fmt.Errorf("select node service release: %w", err)
	}
	return nil
}

func writeJSONAtomic(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode node service install record: %w", err)
	}
	content = append(content, '\n')
	if err := writeAtomic(path, content, 0o600); err != nil {
		return fmt.Errorf("write node service install record: %w", err)
	}
	return nil
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".node-service-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func removeExact(path string, allowLink bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect node service artifact: %w", err)
	}
	if allowLink {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("%w: refusing to remove non-symlink artifact %s", ErrInvalid, path)
		}
	} else if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: refusing to remove non-file artifact %s", ErrInvalid, path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove node service artifact: %w", err)
	}
	return nil
}

func countReleases(directory string) int {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			count++
		}
	}
	return count
}

func absoluteClean(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func dashboardURL(address string) string {
	return "http://" + address
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
