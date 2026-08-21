package preflight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

const (
	ProviderCodex   = "codex"
	ProviderClaude  = "claude"
	ProviderCopilot = "copilot"

	StatusReady  = "ready"
	StatusFailed = "failed"

	proofMarker    = "SNOWCAT_COCKPIT_PREFLIGHT_OK skills=work-snowcat-queue,review-snowcat-queue tool=list_work"
	maxOutputBytes = 1 << 20
)

var (
	repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	serverPattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
)

type Command struct {
	Executable string
	Arguments  []string
	Directory  string
}

type Result struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
}

type Runner interface {
	Run(context.Context, Command) ([]byte, error)
}

type OSRunner struct{}

func Build(provider, mcpServer, repository, directory string) (Command, error) {
	if !serverPattern.MatchString(mcpServer) {
		return Command{}, fmt.Errorf("MCP server must be a configured server name")
	}
	if !repositoryPattern.MatchString(repository) {
		return Command{}, fmt.Errorf("repository must be an owner/name slug")
	}
	if directory == "" {
		return Command{}, errors.New("preflight directory must not be empty")
	}
	prompt := buildPrompt(mcpServer, repository)
	switch provider {
	case ProviderCodex:
		return Command{
			Executable: "codex",
			Directory:  directory,
			Arguments: []string{
				"exec",
				"--ephemeral",
				"--skip-git-repo-check",
				"--sandbox", "read-only",
				"--cd", directory,
				"--config", fmt.Sprintf(`mcp_servers.%s.enabled_tools=["list_work"]`, mcpServer),
				"--config", fmt.Sprintf(`mcp_servers.%s.required=true`, mcpServer),
				"--config", fmt.Sprintf(`mcp_servers.%s.tools.list_work.approval_mode="approve"`, mcpServer),
				prompt,
			},
		}, nil
	case ProviderClaude:
		return Command{
			Executable: "claude",
			Directory:  directory,
			Arguments: []string{
				"--print",
				prompt,
				"--no-session-persistence",
				"--output-format", "text",
				"--permission-mode", "dontAsk",
				"--allowedTools", fmt.Sprintf("mcp__%s__list_work", mcpServer),
			},
		}, nil
	case ProviderCopilot:
		return Command{
			Executable: "copilot",
			Directory:  directory,
			Arguments: []string{
				"--prompt", prompt,
				"-C", directory,
				fmt.Sprintf("--available-tools=%s-list_work", mcpServer),
				fmt.Sprintf("--allow-tool=%s(list_work)", mcpServer),
				"--no-ask-user",
				"--no-remote",
				"--no-remote-export",
				"--no-auto-update",
				"--disable-builtin-mcps",
				"--output-format", "text",
				"--no-color",
			},
		}, nil
	default:
		return Command{}, fmt.Errorf("unsupported provider %q", provider)
	}
}

func Run(ctx context.Context, provider, mcpServer, repository, directory string, runner Runner) Result {
	result := Result{Provider: provider, Status: StatusFailed, Detail: "preflight could not be built"}
	command, err := Build(provider, mcpServer, repository, directory)
	if err != nil {
		result.Detail = err.Error()
		return result
	}
	output, err := runner.Run(ctx, command)
	if err != nil {
		result.Detail = "provider exited without proving Snowcat MCP readiness"
		return result
	}
	if !containsProof(output) {
		result.Detail = "provider returned no valid preflight proof"
		return result
	}
	result.Status = StatusReady
	result.Detail = "locked skills visible; Snowcat list_work succeeded"
	return result
}

func (OSRunner) Run(ctx context.Context, command Command) ([]byte, error) {
	path, err := exec.LookPath(command.Executable)
	if err != nil {
		return nil, err
	}
	process := exec.CommandContext(ctx, path, command.Arguments...)
	process.Dir = command.Directory
	process.Env = os.Environ()
	var output cappedBuffer
	process.Stdout = &output
	process.Stderr = &output
	if err := process.Run(); err != nil {
		return nil, err
	}
	if output.overflow {
		return nil, errors.New("provider output exceeded preflight limit")
	}
	return output.Bytes(), nil
}

func buildPrompt(mcpServer, repository string) string {
	return fmt.Sprintf(`Snowcat Cockpit read-only provider preflight.

This is not queue work. Do not invoke a queue skill and do not read provider configuration, credentials, environment variables, or unrelated files.

Confirm that the available skill catalog includes both work-snowcat-queue and review-snowcat-queue. Then call the list_work tool from the %q MCP server exactly once with repository %q, status "queued", and limit 1. Do not call any other tool. In particular, do not claim, release, heartbeat, complete, block, requeue, propose, or mutate work.

If both skill names are visible and list_work succeeds, output exactly this single line:
%s

Otherwise output exactly:
SNOWCAT_COCKPIT_PREFLIGHT_FAILED`, mcpServer, repository, proofMarker)
}

func containsProof(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == proofMarker {
			return true
		}
	}
	return false
}

type cappedBuffer struct {
	bytes.Buffer
	overflow bool
}

func (buffer *cappedBuffer) Write(content []byte) (int, error) {
	originalLength := len(content)
	remaining := maxOutputBytes - buffer.Len()
	if remaining <= 0 {
		buffer.overflow = true
		return originalLength, nil
	}
	if len(content) > remaining {
		content = content[:remaining]
		buffer.overflow = true
	}
	_, _ = buffer.Buffer.Write(content)
	return originalLength, nil
}
