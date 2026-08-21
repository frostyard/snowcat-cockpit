package preflight

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

type fakeRunner struct {
	output  []byte
	err     error
	command Command
}

func (runner *fakeRunner) Run(_ context.Context, command Command) ([]byte, error) {
	runner.command = command
	return runner.output, runner.err
}

func TestBuildRestrictsEachProviderToListWork(t *testing.T) {
	t.Parallel()

	tests := []struct {
		provider string
		want     []string
	}{
		{ProviderCodex, []string{`mcp_servers.snowcat.enabled_tools=["list_work"]`, `mcp_servers.snowcat.required=true`}},
		{ProviderClaude, []string{"mcp__snowcat__list_work", "dontAsk"}},
		{ProviderCopilot, []string{"--available-tools=snowcat-list_work", "--allow-tool=snowcat(list_work)"}},
	}
	for _, test := range tests {
		test := test
		t.Run(test.provider, func(t *testing.T) {
			t.Parallel()
			command, err := Build(test.provider, "snowcat", "frostyard/firn", "/tmp/preflight")
			if err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(command.Arguments, "\n")
			for _, wanted := range test.want {
				if !strings.Contains(joined, wanted) {
					t.Fatalf("arguments do not contain %q: %#v", wanted, command.Arguments)
				}
			}
			if strings.Contains(joined, "claim_work") {
				t.Fatalf("arguments expose claim_work: %#v", command.Arguments)
			}
		})
	}
}

func TestBuildPreservesPromptAsOneArgument(t *testing.T) {
	t.Parallel()

	command, err := Build(ProviderCodex, "snowcat", "frostyard/firn", "/tmp/preflight")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(command.Arguments, func(argument string) bool {
		return strings.Contains(argument, proofMarker) && strings.Contains(argument, "frostyard/firn")
	}) {
		t.Fatalf("bounded prompt not present as one argv: %#v", command.Arguments)
	}
}

func TestBuildPlacesClaudePromptBeforeVariadicAllowedTools(t *testing.T) {
	t.Parallel()

	command, err := Build(ProviderClaude, "snowcat", "frostyard/firn", "/tmp/preflight")
	if err != nil {
		t.Fatal(err)
	}
	promptIndex := slices.IndexFunc(command.Arguments, func(argument string) bool {
		return strings.Contains(argument, proofMarker)
	})
	allowedToolsIndex := slices.Index(command.Arguments, "--allowedTools")
	if promptIndex < 0 || allowedToolsIndex < 0 {
		t.Fatalf("missing prompt or --allowedTools: %#v", command.Arguments)
	}
	if promptIndex > allowedToolsIndex {
		t.Fatalf("Claude's variadic --allowedTools would consume the prompt: %#v", command.Arguments)
	}
}

func TestRunRequiresExactProofAndSuccessfulExit(t *testing.T) {
	t.Parallel()

	for name, runner := range map[string]*fakeRunner{
		"proof":          {output: []byte(proofMarker + "\n")},
		"proof-in-prose": {output: []byte("result: " + proofMarker + "\n")},
		"failed-exit":    {output: []byte(proofMarker + "\n"), err: errors.New("exit 1")},
	} {
		name, runner := name, runner
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			result := Run(context.Background(), ProviderCodex, "snowcat", "frostyard/firn", "/tmp/preflight", runner)
			want := StatusFailed
			if name == "proof" {
				want = StatusReady
			}
			if result.Status != want {
				t.Fatalf("status = %q, want %q", result.Status, want)
			}
		})
	}
}

func TestBuildRejectsUnboundedInputs(t *testing.T) {
	t.Parallel()

	for _, repository := range []string{"", "firn", "frostyard/firn extra", "frostyard/firn/other"} {
		if _, err := Build(ProviderCodex, "snowcat", repository, "/tmp/preflight"); err == nil {
			t.Fatalf("accepted repository %q", repository)
		}
	}
	if _, err := Build("other", "snowcat", "frostyard/firn", "/tmp/preflight"); err == nil {
		t.Fatal("accepted unknown provider")
	}
	if _, err := Build(ProviderCodex, "snowcat;claim", "frostyard/firn", "/tmp/preflight"); err == nil {
		t.Fatal("accepted unsafe MCP server name")
	}
}
