package nodeup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
	"github.com/frostyard/snowcat-cockpit/internal/doctor"
	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/nodeservice"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/state"
)

const testKitRevision = "bf6bb0d28a3220ce82d5f2c746312d8468495670"

type fakeService struct {
	installs []nodeservice.InstallRequest
	status   nodeservice.Result
	err      error
	install  nodeservice.Result
}

func (service *fakeService) Install(_ context.Context, request nodeservice.InstallRequest) (nodeservice.Result, error) {
	service.installs = append(service.installs, request)
	service.err = nil
	service.status = service.install
	return service.install, nil
}

func (service *fakeService) Status(context.Context, nodeservice.Paths) (nodeservice.Result, error) {
	return service.status, service.err
}

type fakeNode struct {
	mu        sync.Mutex
	enrolled  []string
	setups    []string
	setupErr  map[string]error
	campaign  campaign.Record
	started   []campaign.Request
	startErr  error
	getErr    error
	startedID string
}

func (node *fakeNode) EnrollRepository(_ context.Context, repository string) (managedrepo.Record, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.enrolled = append(node.enrolled, repository)
	return managedrepo.Record{Repository: repository, Status: managedrepo.StatusPending}, nil
}

func (node *fakeNode) SetupRepository(_ context.Context, repository string) (managedrepo.Record, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	node.setups = append(node.setups, repository)
	if err := node.setupErr[repository]; err != nil {
		return managedrepo.Record{}, err
	}
	return managedrepo.Record{
		Repository: repository,
		Status:     managedrepo.StatusReady,
		Source:     filepath.Join("/managed", strings.ReplaceAll(repository, "/", "-")),
		BaseCommit: strings.Repeat("a", 40),
		Detail:     "managed source refreshed",
	}, nil
}

func (node *fakeNode) Campaign(context.Context) (campaign.Record, error) {
	return node.campaign, node.getErr
}

func (node *fakeNode) StartCampaign(_ context.Context, request campaign.Request) (campaign.Record, error) {
	node.started = append(node.started, request)
	if node.startErr != nil {
		return campaign.Record{}, node.startErr
	}
	return campaign.Record{ID: node.startedID, Status: campaign.StatusStarting, Request: request}, nil
}

type fixture struct {
	runner      *Runner
	service     *fakeService
	node        *fakeNode
	proofs      []PreflightRequest
	proofFail   map[string]int
	steps       []Step
	installs    []string
	kitStatus   string
	kitRevision string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	config, err := Parse([]byte(validConfigJSON()), Defaults{
		StateDirectory: filepath.Join(t.TempDir(), "state"), ObserverEnv: "/config/observer.env", WorkerEnv: "/config/worker.env",
	})
	if err != nil {
		t.Fatal(err)
	}
	healthy := nodeservice.Result{
		Install: nodeservice.Record{
			Unit: nodeservice.UnitName, Release: "v0.2.1-0123456789abcdef", Listen: config.Listen, DashboardURL: "http://127.0.0.1:7686",
			StateDirectory: config.StateDirectory, SkillsDirectory: config.SkillsDirectory(), SourceRoot: config.SourceRoot(),
			EnvironmentPath: filepath.Join(t.TempDir(), "service.env"), ConfigPath: "/config/node.json",
		},
		Service: nodeservice.ServiceState{ActiveState: "active", SubState: "running", MainPID: 42},
		Health:  &nodeservice.Health{Status: "ok", NodeID: "node-0123456789abcdef0123456789abcdef", Version: "v0.2.1"},
	}
	f := &fixture{
		service:     &fakeService{status: healthy, install: healthy},
		node:        &fakeNode{startedID: "campaign-0123456789abcdef"},
		proofFail:   map[string]int{},
		kitStatus:   profile.StatusReady,
		kitRevision: testKitRevision,
	}
	f.runner = &Runner{
		Config: config, ConfigPath: "/config/node.json", Executable: "/dist/snowcat-cockpit", Version: "v0.2.1",
		InstallRoot: "/install", UnitDirectory: "/units", Ambient: map[string]string{"PATH": "/usr/bin"},
		Doctor: func() doctor.Result {
			return doctor.Result{Status: doctor.StatusReady, Checks: []doctor.Check{
				{Name: "git", Required: true, Status: doctor.StatusReady}, {Name: "tmux", Required: true, Status: doctor.StatusReady},
				{Name: "podman", Status: doctor.StatusReady}, {Name: "claude", Status: doctor.StatusReady},
			}}
		},
		Inspect: func(string) profile.Snapshot {
			return profile.Snapshot{Kit: profile.Kit{Status: f.kitStatus, Revision: f.kitRevision}}
		},
		InstallKit: func(directory string) (profile.InstallResult, error) {
			f.installs = append(f.installs, directory)
			f.kitStatus = profile.StatusReady
			return profile.InstallResult{Directory: directory, Status: profile.StatusReady}, nil
		},
		RefreshKit: func(context.Context, string, string, string, time.Time) (profile.RefreshResult, error) {
			return profile.RefreshResult{
				Status: profile.StatusReady, Revision: f.kitRevision, PreviousRevision: f.kitRevision,
			}, nil
		},
		Plan: func(request nodeservice.InstallRequest) (nodeservice.InstallPlan, error) {
			return nodeservice.InstallPlan{
				Release: "v0.2.1-0123456789abcdef", Listen: request.Listen, StateDirectory: request.StateDirectory,
				SkillsDirectory: request.SkillsDirectory, SourceRoot: request.SourceRoot, Environment: "PATH=\"/usr/bin\"\n",
			}, nil
		},
		Service: f.service, Node: f.node,
		Preflight: func(_ context.Context, request PreflightRequest) (state.PreflightReceipt, error) {
			f.proofs = append(f.proofs, request)
			if f.proofFail[request.Provider] > 0 {
				f.proofFail[request.Provider]--
				return state.PreflightReceipt{Provider: request.Provider, MCPServer: request.MCPServer, Status: profile.StatusFailed, Detail: "no proof"}, nil
			}
			return state.PreflightReceipt{Provider: request.Provider, MCPServer: request.MCPServer, Status: profile.StatusReady, Detail: "proved", ExpiresAt: time.Now().Add(15 * time.Minute), KitRevision: f.kitRevision}, nil
		},
		ReadPreflights: func(string) (map[string]state.PreflightReceipt, error) {
			return map[string]state.PreflightReceipt{}, nil
		},
		Now: func() time.Time { return time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC) },
		ReadFile: func(string) ([]byte, error) {
			return []byte("PATH=\"/usr/bin\"\n"), nil
		},
		Rename:  func(string, string) error { return nil },
		Observe: func(step Step) { f.steps = append(f.steps, step) },
	}
	return f
}

func stepStatuses(steps []Step) string {
	parts := make([]string, 0, len(steps))
	for _, step := range steps {
		parts = append(parts, step.Name+"="+step.Status)
	}
	return strings.Join(parts, " ")
}

func TestRunConvergesInOrderAndSkipsWhatIsAlreadyInPlace(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stepStatuses(result.Steps))
	}
	if got := stepStatuses(result.Steps); got != "doctor=ok kit=skipped install=skipped repositories=ok source-kit=skipped preflight=ok campaign=ok status=ok" {
		t.Fatalf("steps = %s", got)
	}
	if len(f.service.installs) != 0 {
		t.Fatalf("install must be skipped when the plan matches: %#v", f.service.installs)
	}
	if strings.Join(f.node.enrolled, ",") != "frostyard/clix,frostyard/snowcat" && strings.Join(f.node.enrolled, ",") != "frostyard/snowcat,frostyard/clix" {
		t.Fatalf("enrolled = %v", f.node.enrolled)
	}
	if len(result.Repositories) != 2 || result.Repositories[0].Repository != "frostyard/clix" || result.Repositories[0].Status != managedrepo.StatusReady {
		t.Fatalf("repositories = %#v", result.Repositories)
	}
	if len(f.proofs) != 3 || f.proofs[0].Provider != "codex" || f.proofs[2].MCPServer != "snowcat-mcp" || f.proofs[0].Repository != "frostyard/clix" {
		t.Fatalf("proofs = %#v", f.proofs)
	}
	if len(f.node.started) != 1 || f.node.started[0].Reviewer.MCPServer != "snowcat-mcp" || result.Campaign == nil || result.Campaign.ID != "campaign-0123456789abcdef" {
		t.Fatalf("campaign start = %#v result = %#v", f.node.started, result.Campaign)
	}
	if result.DashboardURL != "http://127.0.0.1:7686" || result.Node == nil {
		t.Fatalf("result = %#v", result)
	}
}

func TestRunRefreshesWorkerKitFromPreparedSnowcatCommitBeforePreflight(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	var source, revision, target string
	f.runner.RefreshKit = func(_ context.Context, gotSource, gotRevision, gotTarget string, _ time.Time) (profile.RefreshResult, error) {
		source, revision, target = gotSource, gotRevision, gotTarget
		previous := f.kitRevision
		f.kitRevision = gotRevision
		return profile.RefreshResult{
			Status: profile.StatusReady, PreviousRevision: previous, Revision: gotRevision,
			RetainedDirectory: gotTarget + ".previous",
		}, nil
	}
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stepStatuses(result.Steps))
	}
	if source != "/managed/frostyard-snowcat" || revision != strings.Repeat("a", 40) || target != f.runner.Config.SkillsDirectory() {
		t.Fatalf("refresh arguments = %q %q %q", source, revision, target)
	}
	if result.Steps[4].Name != "source-kit" || result.Steps[4].Status != StepOK || !strings.Contains(result.Steps[4].Detail, "managed Snowcat revision") {
		t.Fatalf("source-kit step = %#v", result.Steps[4])
	}
	for _, proof := range f.proofs {
		if proof.SkillsDirectory != target {
			t.Fatalf("preflight did not use active kit: %#v", proof)
		}
	}
}

func TestRunRetainsLastGoodKitWhenManagedSkillBytesAreUnavailable(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.runner.RefreshKit = func(context.Context, string, string, string, time.Time) (profile.RefreshResult, error) {
		return profile.RefreshResult{}, fmt.Errorf("%w: offline", profile.ErrSourceUnavailable)
	}
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stepStatuses(result.Steps))
	}
	if result.Steps[4].Name != "source-kit" || result.Steps[4].Status != StepSkipped || !strings.Contains(result.Steps[4].Detail, "last-good") {
		t.Fatalf("source-kit step = %#v", result.Steps[4])
	}
}

func TestRunInstallsWhenReleaseOrEnvironmentDiffers(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		mutate func(*fixture)
		reason string
	}{
		{"no record", func(f *fixture) {
			f.service.status = nodeservice.Result{}
			f.service.err = errors.New("read node service install record: missing")
		}, "no install record"},
		{"unhealthy", func(f *fixture) { f.service.err = nodeservice.ErrUnhealthy }, "not healthy"},
		{"release differs", func(f *fixture) { f.service.status.Install.Release = "v0.2.0-fedcba9876543210" }, "release"},
		{"environment differs", func(f *fixture) {
			f.runner.ReadFile = func(string) ([]byte, error) { return []byte("PATH=\"/other\"\n"), nil }
		}, "environment differs"},
		{"config path differs", func(f *fixture) { f.service.status.Install.ConfigPath = "/elsewhere/node.json" }, "does not name this configuration"},
		{"state path differs", func(f *fixture) { f.service.status.Install.StateDirectory = "/tmp/elsewhere" }, "path differs"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			testCase.mutate(f)
			result, err := f.runner.Run(context.Background())
			if err != nil {
				t.Fatalf("run: %v\n%s", err, stepStatuses(result.Steps))
			}
			if len(f.service.installs) != 1 {
				t.Fatalf("installs = %d", len(f.service.installs))
			}
			request := f.service.installs[0]
			if request.ConfigPath != "/config/node.json" || request.Environment["SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE"] != validImage || request.Environment["SNOWCAT_COCKPIT_DOCKER_CLAUDE_IMAGE"] != validImage || request.Environment["CODEX_HOME"] != "/home/operator/.codex" || request.SourceRoot != f.runner.Config.SourceRoot() {
				t.Fatalf("install request = %#v", request)
			}
			if step := result.Steps[2]; step.Name != "install" || step.Status != StepOK || !strings.Contains(step.Detail, testCase.reason) {
				t.Fatalf("install step = %#v", step)
			}
		})
	}
}

func TestRunMovesDriftedKitAsideAndInstallsMissingKit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.kitStatus = profile.StatusDrifted
	var renamed [2]string
	f.runner.Rename = func(from, to string) error { renamed = [2]string{from, to}; return nil }
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	skills := f.runner.Config.SkillsDirectory()
	if renamed[0] != skills || renamed[1] != skills+".pre-v0.2.1.20260825T000000Z" {
		t.Fatalf("renamed = %v", renamed)
	}
	if len(f.installs) != 1 || f.installs[0] != skills || result.Steps[1].Status != StepOK || !strings.Contains(result.Steps[1].Detail, "retained") {
		t.Fatalf("kit step = %#v installs = %v", result.Steps[1], f.installs)
	}

	g := newFixture(t)
	g.kitStatus = profile.StatusMissing
	renameCalled := false
	g.runner.Rename = func(string, string) error { renameCalled = true; return nil }
	if _, err := g.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if renameCalled || len(g.installs) != 1 {
		t.Fatalf("missing kit: rename = %v installs = %v", renameCalled, g.installs)
	}
}

func TestRunFailsClosedOnDriftedSourceBackedKit(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.kitStatus = profile.StatusDrifted
	skills := f.runner.Config.SkillsDirectory()
	if err := os.MkdirAll(skills, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(".generations", strings.Repeat("a", 40)), filepath.Join(skills, ".active")); err != nil {
		t.Fatal(err)
	}
	renamed := false
	f.runner.Rename = func(string, string) error { renamed = true; return nil }
	result, err := f.runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "integrity failed") {
		t.Fatalf("run error = %v", err)
	}
	if len(result.Steps) != 2 || result.Steps[1].Name != "kit" || result.Steps[1].Status != StepFailed {
		t.Fatalf("steps = %#v", result.Steps)
	}
	if renamed || len(f.installs) != 0 {
		t.Fatalf("source-backed drift was replaced: renamed=%t installs=%v", renamed, f.installs)
	}
}

func TestRunRealKitInstallUsesTempDir(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.runner.Inspect = profile.Inspect
	f.runner.InstallKit = profile.InstallKit
	f.runner.Rename = os.Rename
	skills := f.runner.Config.SkillsDirectory()
	if _, err := f.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := profile.Inspect(skills); snapshot.Kit.Status != profile.StatusReady {
		t.Fatalf("kit after install = %#v", snapshot.Kit)
	}
	if err := os.WriteFile(filepath.Join(skills, "review-snowcat-queue", "SKILL.md"), []byte("drifted"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshot := profile.Inspect(skills); snapshot.Kit.Status != profile.StatusReady {
		t.Fatalf("kit after drift repair = %#v", snapshot.Kit)
	}
	entries, err := os.ReadDir(filepath.Dir(skills))
	if err != nil {
		t.Fatal(err)
	}
	retained := 0
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "worker-kit.pre-v0.2.1.") {
			retained++
		}
	}
	if retained != 1 {
		t.Fatalf("drifted kit was not retained: %v", entries)
	}
}

func TestRunReusesCurrentReceiptsAndRetriesOnce(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.runner.ReadPreflights = func(string) (map[string]state.PreflightReceipt, error) {
		return map[string]state.PreflightReceipt{
			"codex":   {Provider: "codex", MCPServer: "snowcat", Status: profile.StatusReady, KitRevision: testKitRevision, ExpiresAt: f.runner.Now().Add(time.Minute)},
			"claude":  {Provider: "claude", MCPServer: "snowcat", Status: profile.StatusReady, KitRevision: "0000000000000000000000000000000000000000", ExpiresAt: f.runner.Now().Add(time.Minute)},
			"copilot": {Provider: "copilot", MCPServer: "other", Status: profile.StatusReady, KitRevision: testKitRevision, ExpiresAt: f.runner.Now().Add(time.Minute)},
		}, nil
	}
	f.proofFail["claude"] = 1
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatalf("run: %v\n%s", err, stepStatuses(result.Steps))
	}
	providers := make([]string, 0, len(f.proofs))
	for _, proof := range f.proofs {
		providers = append(providers, proof.Provider)
	}
	if strings.Join(providers, ",") != "claude,claude,copilot" {
		t.Fatalf("proofs = %v", providers)
	}
	if len(result.Preflights) != 3 || result.Preflights[0].Detail != "current receipt reused" {
		t.Fatalf("preflights = %#v", result.Preflights)
	}
}

func TestRunAbortsBeforeCampaignWhenProofStillFails(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.proofFail["copilot"] = 2
	result, err := f.runner.Run(context.Background())
	if err == nil || !errors.Is(err, ErrConverge) || !strings.Contains(err.Error(), "copilot") {
		t.Fatalf("err = %v", err)
	}
	if got := stepStatuses(result.Steps); got != "doctor=ok kit=skipped install=skipped repositories=ok source-kit=skipped preflight=failed" {
		t.Fatalf("steps = %s", got)
	}
	if len(f.node.started) != 0 {
		t.Fatal("campaign must not start without a live proof")
	}
}

func TestRunLeavesAnActiveCampaignAlone(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.node.campaign = campaign.Record{ID: "campaign-live", Status: campaign.StatusRunning}
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.node.started) != 0 || result.Campaign == nil || result.Campaign.ID != "campaign-live" || result.Steps[6].Status != StepSkipped {
		t.Fatalf("campaign step = %#v started = %v", result.Steps[6], f.node.started)
	}

	g := newFixture(t)
	g.node.campaign = campaign.Record{ID: "campaign-old", Status: campaign.StatusStopped}
	if _, err := g.runner.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(g.node.started) != 1 {
		t.Fatal("a stopped campaign must be replaced")
	}
}

func TestRunFailsDoctorForMissingRequiredOrAdapterTools(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.runner.Doctor = func() doctor.Result {
		return doctor.Result{Checks: []doctor.Check{{Name: "git", Required: true, Status: doctor.StatusReady}, {Name: "tmux", Required: true, Status: doctor.StatusMissing}, {Name: "podman", Status: doctor.StatusMissing}}}
	}
	result, err := f.runner.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "tmux") || !strings.Contains(err.Error(), "podman") || len(result.Steps) != 1 || result.Steps[0].Status != StepFailed {
		t.Fatalf("err = %v steps = %#v", err, result.Steps)
	}
	if len(f.installs) != 0 || len(f.service.installs) != 0 || len(f.node.enrolled) != 0 {
		t.Fatal("doctor failure must stop before any change")
	}
}

func TestRunReportsFailedRepositoriesWithoutAborting(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.node.setupErr = map[string]error{"frostyard/snowcat": errors.New("dirty working tree")}
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Repositories[1].Status != managedrepo.StatusFailed || !strings.Contains(result.Repositories[1].Detail, "dirty") || !strings.Contains(result.Steps[3].Detail, "1 of 2") {
		t.Fatalf("repositories = %#v step = %#v", result.Repositories, result.Steps[3])
	}

	g := newFixture(t)
	g.node.setupErr = map[string]error{"frostyard/snowcat": errors.New("x"), "frostyard/clix": errors.New("y")}
	if _, err := g.runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "0 of 2") {
		t.Fatalf("all-failed err = %v", err)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	t.Parallel()

	f := newFixture(t)
	f.kitStatus = profile.StatusDrifted
	f.service.status.Install.Release = "v0.2.0-fedcba9876543210"
	f.runner.DryRun = true
	result, err := f.runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := stepStatuses(result.Steps); got != "doctor=ok kit=planned install=planned repositories=planned source-kit=planned preflight=planned campaign=planned status=planned" {
		t.Fatalf("steps = %s", got)
	}
	if len(f.installs) != 0 || len(f.service.installs) != 0 || len(f.node.enrolled) != 0 || len(f.proofs) != 0 || len(f.node.started) != 0 || !result.DryRun {
		t.Fatal("dry run must not change anything")
	}
	if !strings.Contains(result.Steps[2].Detail, "stops any active campaign") {
		t.Fatalf("install plan detail = %q", result.Steps[2].Detail)
	}
}

func TestRunRejectsIncompleteDependencies(t *testing.T) {
	t.Parallel()

	runner := &Runner{}
	if _, err := runner.Run(context.Background()); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
}
