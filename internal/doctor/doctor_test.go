package doctor

import (
	"errors"
	"testing"
)

func TestRunOnlyRequiredToolsAffectOverallStatus(t *testing.T) {
	t.Parallel()

	available := map[string]bool{"git": true, "tmux": true, "codex": true}
	result := run(func(name string) (string, error) {
		if available[name] {
			return "/test/bin/" + name, nil
		}
		return "", errors.New("missing")
	})
	if result.Status != StatusReady {
		t.Fatalf("status = %q, want ready", result.Status)
	}
	if len(result.Checks) != len(tools) {
		t.Fatalf("checks = %d, want %d", len(result.Checks), len(tools))
	}
}

func TestRunReportsMissingRequiredToolAsDegraded(t *testing.T) {
	t.Parallel()

	result := run(func(name string) (string, error) {
		if name == "tmux" {
			return "", errors.New("missing")
		}
		return "/test/bin/" + name, nil
	})
	if result.Status != StatusDegraded {
		t.Fatalf("status = %q, want degraded", result.Status)
	}
	for _, check := range result.Checks {
		if check.Name == "tmux" && check.Status != StatusMissing {
			t.Fatalf("tmux status = %q, want missing", check.Status)
		}
	}
}
