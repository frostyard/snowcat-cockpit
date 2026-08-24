package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
	"github.com/frostyard/snowcat-cockpit/internal/doctor"
	"github.com/frostyard/snowcat-cockpit/internal/managedrepo"
	"github.com/frostyard/snowcat-cockpit/internal/preflight"
	"github.com/frostyard/snowcat-cockpit/internal/profile"
	"github.com/frostyard/snowcat-cockpit/internal/queueview"
	"github.com/frostyard/snowcat-cockpit/internal/state"
	"github.com/frostyard/snowcat-cockpit/internal/web"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

var (
	version = "development"
	commit  = "none"
	date    = "unknown"
	builtBy = "unknown"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}

	switch args[0] {
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "install-kit":
		return runInstallKit(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "profiles":
		return runProfiles(args[1:], stdout, stderr)
	case "preflight":
		return runPreflight(args[1:], stdout, stderr)
	case "workers":
		return runWorkers(args[1:], stdout, stderr)
	case "worker":
		return runWorker(args[1:], stdout, stderr)
	case "version":
		if len(args) != 1 {
			fmt.Fprintln(stderr, "version accepts no arguments")
			return 2
		}
		fmt.Fprintf(stdout, "%s (commit=%s date=%s builtBy=%s)\n", version, commit, date, builtBy)
		return 0
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func runWorkers(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("workers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write managed workers as JSON")
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory containing managed-worker state")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the locked Snowcat worker kit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "workers accepts no positional arguments")
		return 2
	}
	manager, err := newWorkerManager(*stateDirectory, *skillsDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "open managed-worker state: %v\n", err)
		return 1
	}
	records, err := manager.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "list managed workers: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(records); err != nil {
			fmt.Fprintf(stderr, "write managed workers: %v\n", err)
			return 1
		}
		return 0
	}
	if len(records) == 0 {
		fmt.Fprintln(stdout, "no managed workers")
		return 0
	}
	fmt.Fprintln(stdout, "WORKER | STATUS | ADAPTER | PROVIDER | MODEL | ROLE | REPOSITORY | WORKSPACE")
	for _, record := range records {
		fmt.Fprintf(stdout, "%s | %s | %s | %s | %s | %s | %s | %s\n", record.ID, record.Status, record.Adapter, record.Provider, record.Model, record.Role, record.Repository, record.Workspace)
	}
	return 0
}

func runWorker(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "worker requires launch, target, push-target, observe, attach, stop, or cleanup")
		return 2
	}
	switch args[0] {
	case "launch":
		return runWorkerLaunch(args[1:], stdout, stderr)
	case "target":
		return runWorkerTarget(args[1:], stdout, stderr)
	case "push-target":
		return runWorkerPushTarget(args[1:], stdout, stderr)
	case "observe":
		return runWorkerObserve(args[1:], stdout, stderr)
	case "attach", "stop", "cleanup":
		return runWorkerAction(args[0], args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown worker action %q\n", args[0])
		return 2
	}
}

func runWorkerTarget(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("worker target", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workerID := flags.String("worker", "", "managed worker ID")
	repository := flags.String("repository", "", "claimed Snowcat repository as owner/name")
	itemID := flags.String("item", "", "claimed Snowcat item ID")
	kind := flags.String("kind", "", "claimed pull-request-bound work kind")
	pullRequestURL := flags.String("pull-request", "", "bound GitHub pull-request URL")
	headSHA := flags.String("head", "", "bound 40-hex pull-request head")
	jsonOutput := flags.Bool("json", false, "write the prepared target as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "worker target accepts no positional arguments")
		return 2
	}
	directory, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "resolve worker target directory: %v\n", err)
		return 1
	}
	target, err := worker.PrepareTarget(context.Background(), worker.TargetRequest{
		WorkerID: *workerID, Repository: *repository, ItemID: *itemID, Kind: *kind,
		PullRequestURL: *pullRequestURL, HeadSHA: *headSHA,
	}, directory, worker.OSRunner{}, exec.LookPath)
	if err != nil {
		fmt.Fprintf(stderr, "prepare worker target: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(target); err != nil {
			fmt.Fprintf(stderr, "write worker target: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s prepared %s at %s (%s)\n", target.WorkerID, target.PullRequestURL, target.BoundHead, target.Mode)
	if target.Mode == worker.TargetModeBranch {
		fmt.Fprintln(stdout, "Use the exact push-target helper named in the worker prompt for every push.")
	}
	return 0
}

func runWorkerPushTarget(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("worker push-target", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workerID := flags.String("worker", "", "managed worker ID")
	jsonOutput := flags.Bool("json", false, "write the push result as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "worker push-target accepts no positional arguments")
		return 2
	}
	directory, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "resolve worker target directory: %v\n", err)
		return 1
	}
	result, err := worker.PushTarget(context.Background(), *workerID, directory, worker.OSRunner{}, exec.LookPath)
	if err != nil {
		fmt.Fprintf(stderr, "push worker target: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(result); err != nil {
			fmt.Fprintf(stderr, "write worker target push: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "pushed %s from %s to %s on %s\n", result.PullRequestURL, result.PreviousHead, result.PushedHead, result.TargetBranch)
	return 0
}

func runWorkerObserve(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("worker observe", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory containing managed-worker state")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the locked Snowcat worker kit")
	jsonOutput := flags.Bool("json", false, "write the work-attempt observation as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "worker observe requires one worker ID")
		return 2
	}
	manager, err := newWorkerManager(*stateDirectory, *skillsDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "open managed-worker state: %v\n", err)
		return 1
	}
	record, err := manager.Get(context.Background(), flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "observe managed worker: %v\n", err)
		return 1
	}
	observer, err := queueObserverFromLookup(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configure Snowcat worker observation: %v\n", err)
		return 1
	}
	if observer == nil {
		fmt.Fprintln(stderr, "Snowcat worker observation is not configured")
		return 1
	}
	observation, err := observer.ObserveWorker(context.Background(), record.Repository, record.ID)
	if err != nil {
		fmt.Fprintf(stderr, "observe managed worker: %v\n", err)
		return 1
	}
	if *jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(observation); err != nil {
			fmt.Fprintf(stderr, "write worker observation: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "%s work: %s\n", observation.WorkerID, observation.Status)
	fmt.Fprintln(stdout, observation.Detail)
	if observation.ItemID != "" {
		fmt.Fprintf(stdout, "Item: %s (%s, %s)\n", observation.ItemID, observation.Kind, observation.ItemStatus)
	}
	return 0
}

func runWorkerLaunch(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("worker launch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	adapter := flags.String("adapter", worker.AdapterHost, "execution adapter: host or oci")
	runtimeName := flags.String("runtime", "", "OCI runtime: podman or docker (defaults to podman)")
	providerID := flags.String("provider", "", "provider to launch: codex, claude, or copilot")
	role := flags.String("role", "", "worker role: discoverer, implementer, or reviewer")
	repository := flags.String("repository", "", "Snowcat repository filter as owner/name")
	source := flags.String("source", "", "existing local Git working tree")
	baseRef := flags.String("base-ref", "HEAD", "local commit-ish used to create the worktree")
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory for non-secret node and worker state")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the locked Snowcat worker kit")
	jsonOutput := flags.Bool("json", false, "write the worker record as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "worker launch accepts no positional arguments")
		return 2
	}
	manager, err := newWorkerManager(*stateDirectory, *skillsDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "open managed-worker state: %v\n", err)
		return 1
	}
	record, err := manager.Launch(context.Background(), worker.LaunchRequest{
		Adapter: *adapter, Runtime: *runtimeName, Provider: *providerID, Role: *role, Repository: *repository, Source: *source, BaseRef: *baseRef,
	})
	if err != nil {
		fmt.Fprintf(stderr, "launch managed worker: %v\n", err)
		return 1
	}
	return writeWorkerRecord(stdout, stderr, record, *jsonOutput)
}

func runWorkerAction(action string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("worker "+action, flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory containing managed-worker state")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the locked Snowcat worker kit")
	jsonOutput := flags.Bool("json", false, "write the worker record as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintf(stderr, "worker %s requires one worker ID\n", action)
		return 2
	}
	manager, err := newWorkerManager(*stateDirectory, *skillsDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "open managed-worker state: %v\n", err)
		return 1
	}
	workerID := flags.Arg(0)
	if action == "attach" {
		command, err := manager.AttachCommand(context.Background(), workerID)
		if err != nil {
			fmt.Fprintf(stderr, "attach managed worker: %v\n", err)
			return 1
		}
		argv := append([]string{command.Name}, command.Arguments...)
		if err := syscall.Exec(command.Name, argv, command.Env); err != nil {
			fmt.Fprintf(stderr, "attach managed worker: %v\n", err)
			return 1
		}
		return 0
	}
	var record worker.Record
	if action == "stop" {
		record, err = manager.Stop(context.Background(), workerID)
	} else {
		record, err = manager.Cleanup(context.Background(), workerID)
	}
	if err != nil {
		fmt.Fprintf(stderr, "%s managed worker: %v\n", action, err)
		return 1
	}
	return writeWorkerRecord(stdout, stderr, record, *jsonOutput)
}

func writeWorkerRecord(stdout, stderr io.Writer, record worker.Record, jsonOutput bool) int {
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(record); err != nil {
			fmt.Fprintf(stderr, "write managed worker: %v\n", err)
			return 1
		}
		return 0
	}
	execution := record.Adapter
	if record.Adapter == worker.AdapterOCI {
		execution = fmt.Sprintf("%s, %s %s", record.Adapter, record.Runtime, record.RuntimePosture)
	}
	fmt.Fprintf(stdout, "%s %s (%s)\n", record.ID, record.Status, execution)
	fmt.Fprintf(stdout, "Workspace: %s\nBranch: %s\n", record.Workspace, record.Branch)
	return 0
}

func runPreflight(args []string, stdout, stderr io.Writer) int {
	return runPreflightWithRunner(args, stdout, stderr, preflight.OSRunner{})
}

func runPreflightWithRunner(args []string, stdout, stderr io.Writer, runner preflight.Runner) int {
	flags := flag.NewFlagSet("preflight", flag.ContinueOnError)
	flags.SetOutput(stderr)
	providerID := flags.String("provider", "", "provider to validate: codex, claude, or copilot")
	mcpServer := flags.String("mcp-server", "snowcat", "configured Snowcat MCP server name for this provider")
	repository := flags.String("repository", "", "Snowcat repository filter as owner/name")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the Snowcat worker kit")
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory for non-secret node state")
	timeout := flags.Duration("timeout", 2*time.Minute, "maximum provider preflight duration")
	jsonOutput := flags.Bool("json", false, "write the preflight result as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "preflight accepts no positional arguments")
		return 2
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		fmt.Fprintln(stderr, "preflight timeout must be greater than zero and at most 10m")
		return 2
	}
	if _, err := preflight.Build(*providerID, *mcpServer, *repository, "/preflight-validation"); err != nil {
		fmt.Fprintf(stderr, "invalid preflight: %v\n", err)
		return 2
	}

	structural := profile.Inspect(*skillsDirectory)
	var selected *profile.Provider
	for index := range structural.Providers {
		if structural.Providers[index].ID == *providerID {
			selected = &structural.Providers[index]
			break
		}
	}
	if selected == nil || selected.Executable.Status != profile.StatusReady || selected.SkillKit.Status != profile.StatusReady {
		fmt.Fprintf(stderr, "provider %s is not structurally ready; run profiles first\n", *providerID)
		return 1
	}
	if _, err := state.Open(*stateDirectory); err != nil {
		fmt.Fprintf(stderr, "open node state: %v\n", err)
		return 1
	}
	workspace, err := os.MkdirTemp(*stateDirectory, ".preflight-")
	if err != nil {
		fmt.Fprintf(stderr, "create preflight workspace: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	for _, providerSkills := range []string{
		filepath.Join(workspace, ".agents", "skills"),
		filepath.Join(workspace, ".claude", "skills"),
	} {
		if _, err := profile.InstallKit(providerSkills); err != nil {
			fmt.Fprintf(stderr, "seed preflight worker kit: %v\n", err)
			return 1
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	checkedAt := time.Now().UTC()
	result := preflight.Run(ctx, *providerID, *mcpServer, *repository, workspace, runner)
	expiresAt := checkedAt
	if result.Status == preflight.StatusReady {
		expiresAt = checkedAt.Add(15 * time.Minute)
	}
	receipt := state.PreflightReceipt{
		Provider:    result.Provider,
		MCPServer:   *mcpServer,
		Status:      result.Status,
		Detail:      result.Detail,
		CheckedAt:   checkedAt,
		ExpiresAt:   expiresAt,
		KitRevision: profile.LockedManifest().Source.Revision,
	}
	if err := state.WritePreflight(*stateDirectory, receipt); err != nil {
		fmt.Fprintf(stderr, "write preflight receipt: %v\n", err)
		return 1
	}
	if *jsonOutput {
		output := struct {
			Provider  string    `json:"provider"`
			MCPServer string    `json:"mcpServer"`
			Status    string    `json:"status"`
			Detail    string    `json:"detail"`
			CheckedAt time.Time `json:"checkedAt"`
			ExpiresAt time.Time `json:"expiresAt"`
		}{receipt.Provider, receipt.MCPServer, receipt.Status, receipt.Detail, receipt.CheckedAt, receipt.ExpiresAt}
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(output); err != nil {
			fmt.Fprintf(stderr, "write preflight result: %v\n", err)
			return 1
		}
	} else {
		fmt.Fprintf(stdout, "%s Snowcat MCP preflight (%s): %s\n", selected.Label, *mcpServer, result.Status)
		fmt.Fprintln(stdout, result.Detail)
		if result.Status == preflight.StatusReady {
			fmt.Fprintf(stdout, "Valid until: %s\n", expiresAt.Format(time.RFC3339))
		}
	}
	if result.Status != preflight.StatusReady {
		return 1
	}
	return 0
}

func runInstallKit(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("install-kit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the installation result as JSON")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "destination for the locked Snowcat worker kit")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "install-kit accepts no positional arguments")
		return 2
	}

	result, err := profile.InstallKit(*skillsDirectory)
	if *jsonOutput {
		if writeErr := profile.WriteInstallJSON(stdout, result); writeErr != nil {
			fmt.Fprintf(stderr, "write installation result: %v\n", writeErr)
			return 1
		}
	} else {
		profile.WriteInstallText(stdout, result)
	}
	if err != nil {
		fmt.Fprintf(stderr, "install worker kit: %v\n", err)
		return 1
	}
	return 0
}

func runProfiles(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("profiles", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the profile result as JSON")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the Snowcat worker kit")
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory containing non-secret preflight receipts")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "profiles accepts no positional arguments")
		return 2
	}

	snapshot, err := loadProfileSnapshot(*skillsDirectory, *stateDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "read profile state: %v\n", err)
		return 1
	}
	if *jsonOutput {
		if err := profile.WriteJSON(stdout, snapshot); err != nil {
			fmt.Fprintf(stderr, "write profile result: %v\n", err)
			return 1
		}
	} else {
		profile.WriteText(stdout, snapshot)
	}
	if snapshot.Status == profile.StatusMissing {
		return 1
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the readiness result as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "doctor accepts no positional arguments")
		return 2
	}

	result := doctor.Run()
	if *jsonOutput {
		if err := doctor.WriteJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "write doctor result: %v\n", err)
			return 1
		}
	} else {
		doctor.WriteText(stdout, result)
	}
	if result.Status == doctor.StatusDegraded {
		return 1
	}
	return 0
}

func runServe(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	listenAddress := flags.String("listen", "127.0.0.1:7682", "loopback address for the dashboard")
	stateDirectory := flags.String("state-dir", defaultStateDir(), "directory for non-secret node state")
	skillsDirectory := flags.String("skills-dir", defaultSkillsDir(), "directory containing the Snowcat worker kit")
	sourceRoot := flags.String("source-root", "", "directory for Cockpit-managed repository sources (defaults beneath state-dir)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "serve accepts no positional arguments")
		return 2
	}

	if err := validateListenAddress(*listenAddress); err != nil {
		fmt.Fprintf(stderr, "invalid listen address: %v\n", err)
		return 2
	}

	nodeState, err := state.Open(*stateDirectory)
	if err != nil {
		fmt.Fprintf(stderr, "open node state: %v\n", err)
		return 1
	}
	workerManager, err := newWorkerManagerWithNode(*stateDirectory, *skillsDirectory, nodeState)
	if err != nil {
		fmt.Fprintf(stderr, "open managed-worker state: %v\n", err)
		return 1
	}
	defer workerManager.Close()
	queueObserver, err := queueObserverFromLookup(os.Getenv)
	if err != nil {
		fmt.Fprintf(stderr, "configure Snowcat queue observation: %v\n", err)
		return 1
	}
	managedSourceRoot := *sourceRoot
	if managedSourceRoot == "" {
		managedSourceRoot = filepath.Join(*stateDirectory, "sources")
	}
	repositories, err := managedrepo.New(*stateDirectory, managedSourceRoot)
	if err != nil {
		fmt.Fprintf(stderr, "open managed repository catalog: %v\n", err)
		return 1
	}
	var campaigns *campaign.Controller
	if queueObserver != nil {
		campaigns, err = campaign.New(campaign.Config{
			StateDirectory: *stateDirectory,
			Repositories:   repositories,
			Preflights:     &campaignPreflighter{stateDirectory: *stateDirectory, skillsDirectory: *skillsDirectory, runner: preflight.OSRunner{}},
			Queue:          queueObserver,
			Workers:        workerManager,
		})
		if err != nil {
			fmt.Fprintf(stderr, "open board campaign controller: %v\n", err)
			return 1
		}
		defer campaigns.Close()
	}

	listener, err := net.Listen("tcp", *listenAddress)
	if err != nil {
		fmt.Fprintf(stderr, "listen on %s: %v\n", *listenAddress, err)
		return 1
	}

	startedAt := time.Now().UTC()
	server := &http.Server{
		Handler: web.New(web.Config{
			NodeID:    nodeState.NodeID,
			Version:   version,
			StartedAt: startedAt,
			Doctor:    doctor.Run,
			Profiles: func() profile.Snapshot {
				snapshot, err := loadProfileSnapshot(*skillsDirectory, *stateDirectory)
				if err != nil {
					return profile.Inspect(*skillsDirectory)
				}
				return snapshot
			},
			Workers:      workerManager,
			Queue:        queueObserver,
			Attempts:     queueObserver,
			Repositories: repositories,
			Campaigns:    campaigns,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()

	fmt.Fprintf(stdout, "Snowcat Cockpit %s listening on http://%s\n", version, listener.Addr())
	err = server.Serve(listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "serve dashboard: %v\n", err)
		return 1
	}
	return 0
}

type campaignPreflighter struct {
	stateDirectory  string
	skillsDirectory string
	runner          preflight.Runner
}

func (service *campaignPreflighter) Refresh(ctx context.Context, providerID, mcpServer, repository string) (campaign.PreflightResult, error) {
	if _, err := preflight.Build(providerID, mcpServer, repository, "/preflight-validation"); err != nil {
		return campaign.PreflightResult{}, fmt.Errorf("invalid provider preflight: %w", err)
	}
	structural := profile.Inspect(service.skillsDirectory)
	structurallyReady := false
	for _, provider := range structural.Providers {
		if provider.ID == providerID && provider.Executable.Status == profile.StatusReady && provider.SkillKit.Status == profile.StatusReady {
			structurallyReady = true
			break
		}
	}
	if !structurallyReady {
		return campaign.PreflightResult{Status: campaign.StatusDegraded, Detail: "provider is not structurally ready"}, fmt.Errorf("provider %s is not structurally ready", providerID)
	}
	workspace, err := os.MkdirTemp(service.stateDirectory, ".campaign-preflight-")
	if err != nil {
		return campaign.PreflightResult{}, fmt.Errorf("create provider preflight workspace: %w", err)
	}
	defer func() { _ = os.RemoveAll(workspace) }()
	for _, providerSkills := range []string{
		filepath.Join(workspace, ".agents", "skills"),
		filepath.Join(workspace, ".claude", "skills"),
	} {
		if _, err := profile.InstallKit(providerSkills); err != nil {
			return campaign.PreflightResult{}, fmt.Errorf("seed provider preflight skills: %w", err)
		}
	}

	var last campaign.PreflightResult
	for attempt := 1; attempt <= 2; attempt++ {
		attemptContext, cancel := context.WithTimeout(ctx, 2*time.Minute)
		checkedAt := time.Now().UTC()
		result := preflight.Run(attemptContext, providerID, mcpServer, repository, workspace, service.runner)
		cancel()
		expiresAt := checkedAt
		if result.Status == preflight.StatusReady {
			expiresAt = checkedAt.Add(15 * time.Minute)
		}
		receipt := state.PreflightReceipt{
			Provider: result.Provider, MCPServer: mcpServer, Status: result.Status,
			Detail: result.Detail, CheckedAt: checkedAt, ExpiresAt: expiresAt,
			KitRevision: profile.LockedManifest().Source.Revision,
		}
		if err := state.WritePreflight(service.stateDirectory, receipt); err != nil {
			return campaign.PreflightResult{}, fmt.Errorf("write provider preflight receipt: %w", err)
		}
		last = campaign.PreflightResult{Status: result.Status, Detail: result.Detail, ExpiresAt: expiresAt}
		if result.Status == preflight.StatusReady {
			return last, nil
		}
		if ctx.Err() != nil {
			return last, ctx.Err()
		}
	}
	return last, fmt.Errorf("provider preflight failed after two attempts")
}

type snowcatObserver interface {
	queueview.Observer
	queueview.AttemptObserver
}

func queueObserverFromLookup(lookup func(string) string) (snowcatObserver, error) {
	endpoint := lookup("SNOWCAT_COCKPIT_MCP_URL")
	token := lookup("SNOWCAT_COCKPIT_MCP_TOKEN")
	if endpoint == "" && token == "" {
		return nil, nil
	}
	if endpoint == "" {
		return nil, errors.New("SNOWCAT_COCKPIT_MCP_URL is required when queue observation is configured")
	}
	if token == "" {
		return nil, errors.New("SNOWCAT_COCKPIT_MCP_TOKEN is required when queue observation is configured")
	}
	return queueview.NewHTTPObserver(queueview.HTTPConfig{Endpoint: endpoint, Token: token})
}

func loadProfileSnapshot(skillsDirectory, stateDirectory string) (profile.Snapshot, error) {
	stored, err := state.ReadPreflights(stateDirectory)
	if err != nil {
		return profile.Snapshot{}, err
	}
	receipts := make(map[string]profile.PreflightReceipt, len(stored))
	for providerID, receipt := range stored {
		receipts[providerID] = profile.PreflightReceipt{
			Status:      receipt.Status,
			Detail:      receipt.Detail,
			MCPServer:   receipt.MCPServer,
			CheckedAt:   receipt.CheckedAt,
			ExpiresAt:   receipt.ExpiresAt,
			KitRevision: receipt.KitRevision,
		}
	}
	return profile.InspectWithPreflights(skillsDirectory, receipts, time.Now().UTC()), nil
}

func newWorkerManager(stateDirectory, skillsDirectory string) (*worker.Manager, error) {
	nodeState, err := state.Open(stateDirectory)
	if err != nil {
		return nil, err
	}
	return newWorkerManagerWithNode(stateDirectory, skillsDirectory, nodeState)
}

func newWorkerManagerWithNode(stateDirectory, skillsDirectory string, nodeState state.Node) (*worker.Manager, error) {
	targetHelper, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve Cockpit target helper: %w", err)
	}
	return worker.New(worker.Config{
		StateDirectory: stateDirectory,
		NodeID:         nodeState.NodeID,
		TargetHelper:   targetHelper,
		OCI: worker.OCIConfig{
			Images: map[string]string{
				"codex":   firstNonempty(os.Getenv("SNOWCAT_COCKPIT_OCI_CODEX_IMAGE"), os.Getenv("SNOWCAT_COCKPIT_OCI_IMAGE")),
				"claude":  os.Getenv("SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE"),
				"copilot": os.Getenv("SNOWCAT_COCKPIT_OCI_COPILOT_IMAGE"),
			},
			DockerImages: map[string]string{
				"codex":   os.Getenv("SNOWCAT_COCKPIT_DOCKER_CODEX_IMAGE"),
				"claude":  os.Getenv("SNOWCAT_COCKPIT_DOCKER_CLAUDE_IMAGE"),
				"copilot": os.Getenv("SNOWCAT_COCKPIT_DOCKER_COPILOT_IMAGE"),
			},
			DockerAddHost: os.Getenv("SNOWCAT_COCKPIT_DOCKER_ADD_HOST"),
			CodexHome:     defaultCodexHome(),
			ClaudeHome:    defaultClaudeHome(),
			CopilotHome:   defaultCopilotHome(),
			GHConfigDir:   defaultGHConfigDir(),
		},
		Ready: func(providerID string) error {
			snapshot, err := loadProfileSnapshot(skillsDirectory, stateDirectory)
			if err != nil {
				return err
			}
			for _, candidate := range snapshot.Providers {
				if candidate.ID != providerID {
					continue
				}
				if candidate.Status != profile.StatusReady {
					return fmt.Errorf("%s profile is %s", providerID, candidate.Status)
				}
				return nil
			}
			return fmt.Errorf("unknown provider %s", providerID)
		},
	})
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func defaultCodexHome() string {
	if directory := os.Getenv("CODEX_HOME"); directory != "" {
		return directory
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func defaultCopilotHome() string {
	if directory := os.Getenv("COPILOT_HOME"); directory != "" {
		return directory
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".copilot")
}

func defaultClaudeHome() string {
	if directory := os.Getenv("CLAUDE_CONFIG_DIR"); directory != "" {
		return directory
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func defaultGHConfigDir() string {
	if directory := os.Getenv("GH_CONFIG_DIR"); directory != "" {
		return directory
	}
	if root := os.Getenv("XDG_CONFIG_HOME"); root != "" {
		return filepath.Join(root, "gh")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "gh")
}

func defaultStateDir() string {
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		return filepath.Join(root, "snowcat-cockpit")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".snowcat-cockpit-state")
	}
	return filepath.Join(home, ".local", "state", "snowcat-cockpit")
}

func defaultSkillsDir() string {
	if directory := os.Getenv("SNOWCAT_COCKPIT_SKILLS_DIR"); directory != "" {
		return directory
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".agents", "skills")
}

func validateListenAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("expected host:port: %w", err)
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
	default:
		return fmt.Errorf("host %q is not loopback; use 127.0.0.1, ::1, or localhost", host)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port %q must be between 1 and 65535", portText)
	}
	return nil
}

func printUsage(output io.Writer) {
	fmt.Fprintln(output, `Usage:
  snowcat-cockpit doctor [--json]
  snowcat-cockpit install-kit [--json] [--skills-dir <directory>]
  snowcat-cockpit profiles [--json] [--skills-dir <directory>] [--state-dir <directory>]
  snowcat-cockpit preflight --provider <name> --mcp-server <name> --repository <owner/name> [--timeout <duration>]
  snowcat-cockpit workers [--json] [--state-dir <directory>]
  snowcat-cockpit worker launch [--adapter host|oci] [--runtime podman|docker] --provider <name> --role <name> --repository <owner/name> --source <directory> [--base-ref <ref>]
  snowcat-cockpit worker target --worker <id> --repository <owner/name> --item <uuid> --kind <kind> --pull-request <url> --head <sha> [--json]
  snowcat-cockpit worker push-target --worker <id> [--json]
  snowcat-cockpit worker observe|attach|stop|cleanup [options] <worker-id>
  snowcat-cockpit serve [--listen <host:port>] [--state-dir <directory>] [--skills-dir <directory>]
  snowcat-cockpit version
  snowcat-cockpit help`)
}
