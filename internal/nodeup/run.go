package nodeup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
	"github.com/frostyard/snowcat-cockpit/internal/doctor"
	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/nodeservice"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/state"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

// Step statuses.
const (
	StepOK      = "ok"
	StepSkipped = "skipped"
	StepPlanned = "planned"
	StepFailed  = "failed"
)

const (
	repositoryConcurrency = 4
	preflightAttempts     = 2
)

// ErrConverge marks a convergence step that failed after validation passed.
var ErrConverge = errors.New("node convergence failed")

// Step is one convergence step's outcome.
type Step struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// RepositoryResult is one managed repository's setup outcome.
type RepositoryResult struct {
	Repository string `json:"repository"`
	Status     string `json:"status"`
	BaseCommit string `json:"baseCommit,omitempty"`
	Detail     string `json:"detail"`
}

// PreflightResult is one provider's live proof outcome.
type PreflightResult struct {
	Provider  string    `json:"provider"`
	MCPServer string    `json:"mcpServer"`
	Status    string    `json:"status"`
	Detail    string    `json:"detail"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
}

// Result is the structured outcome of one convergence run.
type Result struct {
	ConfigPath   string              `json:"configPath"`
	DryRun       bool                `json:"dryRun"`
	Steps        []Step              `json:"steps"`
	Node         *nodeservice.Result `json:"node,omitempty"`
	Repositories []RepositoryResult  `json:"repositories,omitempty"`
	Preflights   []PreflightResult   `json:"preflights,omitempty"`
	Campaign     *campaign.Record    `json:"campaign,omitempty"`
	DashboardURL string              `json:"dashboardUrl,omitempty"`
}

// Service is the subset of the node service manager convergence uses.
type Service interface {
	Install(context.Context, nodeservice.InstallRequest) (nodeservice.Result, error)
	Status(context.Context, nodeservice.Paths) (nodeservice.Result, error)
}

// NodeClient talks to the running node over its loopback API, which owns
// repository and campaign state.
type NodeClient interface {
	EnrollRepository(context.Context, string) (managedrepo.Record, error)
	SetupRepository(context.Context, string) (managedrepo.Record, error)
	Campaign(context.Context) (campaign.Record, error)
	StartCampaign(context.Context, campaign.Request) (campaign.Record, error)
}

// PreflightRequest names one live proof to run and where to record it.
type PreflightRequest struct {
	Provider        string
	MCPServer       string
	Repository      string
	StateDirectory  string
	SkillsDirectory string
}

// Runner holds a validated configuration and every dependency convergence
// touches, so the sequence is testable without systemd or a live node.
type Runner struct {
	Config        Config
	ConfigPath    string
	Executable    string
	Version       string
	InstallRoot   string
	UnitDirectory string
	Ambient       map[string]string
	DryRun        bool

	Doctor         func() doctor.Result
	Inspect        func(skillsDirectory string) profile.Snapshot
	InstallKit     func(skillsDirectory string) (profile.InstallResult, error)
	Plan           func(nodeservice.InstallRequest) (nodeservice.InstallPlan, error)
	Service        Service
	Node           NodeClient
	Preflight      func(context.Context, PreflightRequest) (state.PreflightReceipt, error)
	ReadPreflights func(stateDirectory string) (map[string]state.PreflightReceipt, error)
	KitRevision    string
	Now            func() time.Time
	ReadFile       func(string) ([]byte, error)
	Rename         func(string, string) error
	Observe        func(Step)
}

// Run converges the host to the configuration. The returned result is
// populated through the failing step when an error is returned.
func (runner *Runner) Run(ctx context.Context) (Result, error) {
	if err := runner.validate(); err != nil {
		return Result{}, err
	}
	result := Result{ConfigPath: runner.ConfigPath, DryRun: runner.DryRun}
	steps := []func(context.Context, *Result) error{
		runner.stepDoctor, runner.stepKit, runner.stepInstall,
		runner.stepRepositories, runner.stepPreflights, runner.stepCampaign, runner.stepStatus,
	}
	for _, step := range steps {
		if err := step(ctx, &result); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (runner *Runner) validate() error {
	switch {
	case runner.Doctor == nil, runner.Inspect == nil, runner.InstallKit == nil, runner.Plan == nil,
		runner.Service == nil, runner.Node == nil, runner.Preflight == nil, runner.ReadPreflights == nil:
		return fmt.Errorf("%w: convergence dependencies are incomplete", ErrInvalid)
	case runner.Executable == "" || runner.Version == "" || runner.InstallRoot == "" || runner.UnitDirectory == "" || runner.KitRevision == "":
		return fmt.Errorf("%w: executable, version, install root, unit directory, and kit revision are required", ErrInvalid)
	}
	if runner.Now == nil {
		runner.Now = time.Now
	}
	if runner.ReadFile == nil {
		runner.ReadFile = os.ReadFile
	}
	if runner.Rename == nil {
		runner.Rename = os.Rename
	}
	return nil
}

func (runner *Runner) record(result *Result, name, status, detail string) {
	step := Step{Name: name, Status: status, Detail: detail}
	result.Steps = append(result.Steps, step)
	if runner.Observe != nil {
		runner.Observe(step)
	}
}

func (runner *Runner) fail(result *Result, name, detail string) error {
	runner.record(result, name, StepFailed, detail)
	return fmt.Errorf("%w: %s: %s", ErrConverge, name, detail)
}

func (runner *Runner) stepDoctor(_ context.Context, result *Result) error {
	report := runner.Doctor()
	ready := make(map[string]bool, len(report.Checks))
	var missing []string
	for _, check := range report.Checks {
		ready[check.Name] = check.Status == doctor.StatusReady
		if check.Required && check.Status != doctor.StatusReady {
			missing = append(missing, check.Name)
		}
	}
	declared := runner.Config.Campaign
	if declared.Adapter == worker.AdapterOCI && !ready[declared.Runtime] {
		missing = append(missing, declared.Runtime)
	}
	if declared.Adapter == worker.AdapterHost {
		for _, pair := range runner.Config.LanePairs() {
			if !ready[pair.Provider] {
				missing = append(missing, pair.Provider)
			}
		}
	}
	if len(missing) > 0 {
		return runner.fail(result, "doctor", "missing on PATH: "+strings.Join(missing, ", "))
	}
	runner.record(result, "doctor", StepOK, fmt.Sprintf("%d checks; required tools and the %s adapter are available", len(report.Checks), declared.Adapter))
	return nil
}

func (runner *Runner) stepKit(_ context.Context, result *Result) error {
	skills := runner.Config.SkillsDirectory()
	snapshot := runner.Inspect(skills)
	switch snapshot.Kit.Status {
	case profile.StatusReady:
		runner.record(result, "kit", StepSkipped, "worker kit ready at "+shortRevision(snapshot.Kit.Revision))
		return nil
	case profile.StatusDrifted:
		aside := fmt.Sprintf("%s.pre-%s.%s", skills, sanitizeVersion(runner.Version), runner.Now().UTC().Format("20060102T150405Z"))
		if runner.DryRun {
			runner.record(result, "kit", StepPlanned, fmt.Sprintf("would move the drifted kit aside to %s and install revision %s", filepath.Base(aside), shortRevision(runner.KitRevision)))
			return nil
		}
		if err := runner.Rename(skills, aside); err != nil {
			return runner.fail(result, "kit", fmt.Sprintf("move drifted worker kit aside: %v", err))
		}
		installed, err := runner.InstallKit(skills)
		if err != nil {
			return runner.fail(result, "kit", fmt.Sprintf("install worker kit: %v", err))
		}
		runner.record(result, "kit", StepOK, fmt.Sprintf("drifted kit retained as %s; installed revision %s (%s)", filepath.Base(aside), shortRevision(runner.KitRevision), installed.Status))
		return nil
	default:
		if runner.DryRun {
			runner.record(result, "kit", StepPlanned, "would install worker kit revision "+shortRevision(runner.KitRevision))
			return nil
		}
		installed, err := runner.InstallKit(skills)
		if err != nil {
			return runner.fail(result, "kit", fmt.Sprintf("install worker kit: %v", err))
		}
		runner.record(result, "kit", StepOK, fmt.Sprintf("installed worker kit revision %s (%s)", shortRevision(runner.KitRevision), installed.Status))
		return nil
	}
}

func (runner *Runner) installRequest() nodeservice.InstallRequest {
	config := runner.Config
	return nodeservice.InstallRequest{
		Executable: runner.Executable, Version: runner.Version, Listen: config.Listen,
		StateDirectory: config.StateDirectory, SkillsDirectory: config.SkillsDirectory(), SourceRoot: config.SourceRoot(),
		ObserverEnv: config.ObserverEnv, WorkerEnv: config.WorkerEnv,
		InstallRoot: runner.InstallRoot, UnitDirectory: runner.UnitDirectory, ConfigPath: runner.ConfigPath,
		Environment: config.ServiceEnvironment(runner.Ambient),
	}
}

func (runner *Runner) paths() nodeservice.Paths {
	return nodeservice.Paths{InstallRoot: runner.InstallRoot, UnitDirectory: runner.UnitDirectory}
}

// installReason reports why an install is needed, or "" when the installed
// service already matches the plan and is healthy.
func (runner *Runner) installReason(ctx context.Context, plan nodeservice.InstallPlan) (string, *nodeservice.Result) {
	status, err := runner.Service.Status(ctx, runner.paths())
	if err != nil {
		if status.Install.Unit == "" {
			return "no install record", nil
		}
		return "service is not healthy: " + err.Error(), &status
	}
	record := status.Install
	switch {
	case record.Release != plan.Release:
		return fmt.Sprintf("release %s differs from installed %s", plan.Release, record.Release), &status
	case record.Listen != plan.Listen:
		return "listen address differs", &status
	case record.StateDirectory != plan.StateDirectory || record.SkillsDirectory != plan.SkillsDirectory || record.SourceRoot != plan.SourceRoot:
		return "state, skills, or source path differs", &status
	case record.ConfigPath != runner.ConfigPath:
		return "install record does not name this configuration", &status
	}
	current, err := runner.ReadFile(record.EnvironmentPath)
	if err != nil {
		return "service environment is unreadable", &status
	}
	if !bytes.Equal(current, []byte(plan.Environment)) {
		return "service environment differs", &status
	}
	return "", &status
}

func (runner *Runner) stepInstall(ctx context.Context, result *Result) error {
	request := runner.installRequest()
	plan, err := runner.Plan(request)
	if err != nil {
		return runner.fail(result, "install", err.Error())
	}
	reason, status := runner.installReason(ctx, plan)
	if reason == "" {
		result.Node = status
		runner.record(result, "install", StepSkipped, "release "+plan.Release+" is installed and healthy")
		return nil
	}
	if runner.DryRun {
		runner.record(result, "install", StepPlanned, fmt.Sprintf("would install release %s (%s); this restarts the node and stops any active campaign", plan.Release, reason))
		return nil
	}
	installed, err := runner.Service.Install(ctx, request)
	if installed.Install.Unit != "" {
		result.Node = &installed
	}
	if err != nil {
		return runner.fail(result, "install", err.Error())
	}
	runner.record(result, "install", StepOK, fmt.Sprintf("installed release %s (%s)", installed.Install.Release, reason))
	return nil
}

func (runner *Runner) stepRepositories(ctx context.Context, result *Result) error {
	repositories := runner.Config.Repositories
	if runner.DryRun {
		runner.record(result, "repositories", StepPlanned, fmt.Sprintf("would enroll and set up %d repositories: %s", len(repositories), strings.Join(repositories, ", ")))
		return nil
	}
	results := make([]RepositoryResult, len(repositories))
	var wait sync.WaitGroup
	slots := make(chan struct{}, repositoryConcurrency)
	for index, repository := range repositories {
		wait.Add(1)
		go func(index int, repository string) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = runner.converge(ctx, repository)
		}(index, repository)
	}
	wait.Wait()
	result.Repositories = results
	ready := 0
	var failed []string
	for _, entry := range results {
		if entry.Status == managedrepo.StatusReady {
			ready++
		} else {
			failed = append(failed, entry.Repository)
		}
	}
	detail := fmt.Sprintf("%d of %d repositories ready", ready, len(results))
	if len(failed) > 0 {
		detail += "; not ready: " + strings.Join(failed, ", ")
	}
	if ready == 0 {
		return runner.fail(result, "repositories", detail)
	}
	runner.record(result, "repositories", StepOK, detail)
	return nil
}

func (runner *Runner) converge(ctx context.Context, repository string) RepositoryResult {
	if _, err := runner.Node.EnrollRepository(ctx, repository); err != nil {
		return RepositoryResult{Repository: repository, Status: managedrepo.StatusFailed, Detail: "enroll: " + err.Error()}
	}
	record, err := runner.Node.SetupRepository(ctx, repository)
	if err != nil {
		return RepositoryResult{Repository: repository, Status: managedrepo.StatusFailed, Detail: "setup: " + err.Error()}
	}
	return RepositoryResult{Repository: record.Repository, Status: record.Status, BaseCommit: record.BaseCommit, Detail: record.Detail}
}

func (runner *Runner) stepPreflights(ctx context.Context, result *Result) error {
	config := runner.Config
	existing, err := runner.ReadPreflights(config.StateDirectory)
	if err != nil {
		return runner.fail(result, "preflight", "read preflight receipts: "+err.Error())
	}
	now := runner.Now().UTC()
	var pending []LanePair
	for _, pair := range config.LanePairs() {
		receipt, ok := existing[pair.Provider]
		if ok && receipt.Status == profile.StatusReady && receipt.MCPServer == pair.MCPServer && receipt.KitRevision == runner.KitRevision && receipt.ExpiresAt.After(now) {
			result.Preflights = append(result.Preflights, PreflightResult{Provider: pair.Provider, MCPServer: pair.MCPServer, Status: receipt.Status, Detail: "current receipt reused", ExpiresAt: receipt.ExpiresAt})
			continue
		}
		pending = append(pending, pair)
	}
	if len(pending) == 0 {
		runner.record(result, "preflight", StepSkipped, "every lane provider holds a current live proof")
		return nil
	}
	names := make([]string, 0, len(pending))
	for _, pair := range pending {
		names = append(names, pair.Provider+"/"+pair.MCPServer)
	}
	if runner.DryRun {
		runner.record(result, "preflight", StepPlanned, "would prove "+strings.Join(names, ", "))
		return nil
	}
	var failed []string
	for _, pair := range pending {
		entry := runner.prove(ctx, pair)
		result.Preflights = append(result.Preflights, entry)
		if entry.Status != profile.StatusReady {
			failed = append(failed, pair.Provider)
		}
	}
	if len(failed) > 0 {
		return runner.fail(result, "preflight", "no live proof for "+strings.Join(failed, ", ")+"; the campaign was not started")
	}
	runner.record(result, "preflight", StepOK, "proved "+strings.Join(names, ", "))
	return nil
}

func (runner *Runner) prove(ctx context.Context, pair LanePair) PreflightResult {
	config := runner.Config
	request := PreflightRequest{
		Provider: pair.Provider, MCPServer: pair.MCPServer, Repository: config.Repositories[0],
		StateDirectory: config.StateDirectory, SkillsDirectory: config.SkillsDirectory(),
	}
	entry := PreflightResult{Provider: pair.Provider, MCPServer: pair.MCPServer, Status: profile.StatusFailed}
	for attempt := 0; attempt < preflightAttempts; attempt++ {
		receipt, err := runner.Preflight(ctx, request)
		if err != nil {
			entry.Detail = err.Error()
			if ctx.Err() != nil {
				return entry
			}
			continue
		}
		entry.Status, entry.Detail, entry.ExpiresAt = receipt.Status, receipt.Detail, receipt.ExpiresAt
		if receipt.Status == profile.StatusReady {
			return entry
		}
	}
	return entry
}

func campaignActive(status string) bool {
	switch status {
	case campaign.StatusStarting, campaign.StatusRunning, campaign.StatusDegraded, campaign.StatusStopping:
		return true
	}
	return false
}

func (runner *Runner) stepCampaign(ctx context.Context, result *Result) error {
	request := runner.Config.CampaignRequest()
	if runner.DryRun {
		runner.record(result, "campaign", StepPlanned, fmt.Sprintf("would start a %s campaign unless one is active (%s discoverer, %s implementer, %s reviewer)", request.Adapter, request.Discoverer.Provider, request.Implementer.Provider, request.Reviewer.Provider))
		return nil
	}
	current, err := runner.Node.Campaign(ctx)
	if err != nil {
		return runner.fail(result, "campaign", "read campaign: "+err.Error())
	}
	if campaignActive(current.Status) {
		result.Campaign = &current
		runner.record(result, "campaign", StepSkipped, fmt.Sprintf("campaign %s is %s; left in place", current.ID, current.Status))
		return nil
	}
	started, err := runner.Node.StartCampaign(ctx, request)
	if err != nil {
		return runner.fail(result, "campaign", "start campaign: "+err.Error())
	}
	result.Campaign = &started
	runner.record(result, "campaign", StepOK, fmt.Sprintf("campaign %s %s", started.ID, started.Status))
	return nil
}

func (runner *Runner) stepStatus(ctx context.Context, result *Result) error {
	if runner.DryRun {
		runner.record(result, "status", StepPlanned, "would verify node health and print the dashboard URL")
		return nil
	}
	status, err := runner.Service.Status(ctx, runner.paths())
	if err != nil {
		return runner.fail(result, "status", err.Error())
	}
	result.Node = &status
	result.DashboardURL = status.Install.DashboardURL
	detail := fmt.Sprintf("%s %s/%s · release %s", status.Install.Unit, status.Service.ActiveState, status.Service.SubState, status.Install.Release)
	if status.Health != nil {
		detail += " · node " + status.Health.NodeID
	}
	runner.record(result, "status", StepOK, detail)
	return nil
}

func shortRevision(revision string) string {
	if len(revision) > 7 {
		return revision[:7]
	}
	return revision
}

func sanitizeVersion(version string) string {
	var output strings.Builder
	for _, char := range version {
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9', char == '.', char == '-', char == '_':
			output.WriteRune(char)
		default:
			output.WriteByte('-')
		}
	}
	if output.Len() == 0 {
		return "unknown"
	}
	return output.String()
}
