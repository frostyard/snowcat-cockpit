package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

const (
	StatusReady    = "ready"
	StatusMissing  = "missing"
	StatusWarning  = "warning"
	StatusDegraded = "degraded"
)

type Check struct {
	Name     string `json:"name"`
	Category string `json:"category"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Action   string `json:"action,omitempty"`
	Required bool   `json:"required"`
}

type Result struct {
	Status string  `json:"status"`
	Checks []Check `json:"checks"`
}

type tool struct {
	name       string
	category   string
	required   bool
	missingTip string
}

var tools = []tool{
	{name: "git", category: "source", required: true, missingTip: "Install Git before launching workers."},
	{name: "tmux", category: "terminal", required: true, missingTip: "Install tmux for durable worker terminals."},
	{name: "ttyd", category: "terminal", missingTip: "Install ttyd only if the legacy browser terminal is needed."},
	{name: "podman", category: "execution", missingTip: "Install rootless Podman to enable OCI workers."},
	{name: "docker", category: "execution", missingTip: "Install Docker to enable the Docker compatibility adapter."},
	{name: "codex", category: "provider", missingTip: "Install Codex to enable Codex worker profiles."},
	{name: "claude", category: "provider", missingTip: "Install Claude Code to enable Claude worker profiles."},
	{name: "copilot", category: "provider", missingTip: "Install GitHub Copilot CLI to enable Copilot worker profiles."},
	{name: "gh", category: "delivery", missingTip: "Install GitHub CLI for pull-request delivery checks."},
}

func Run() Result {
	return run(exec.LookPath)
}

func run(lookPath func(string) (string, error)) Result {
	result := Result{Status: StatusReady, Checks: make([]Check, 0, len(tools))}
	for _, candidate := range tools {
		check := Check{
			Name:     candidate.name,
			Category: candidate.category,
			Required: candidate.required,
		}
		if _, err := lookPath(candidate.name); err != nil {
			check.Status = StatusMissing
			check.Detail = "not found on PATH"
			check.Action = candidate.missingTip
			if candidate.required {
				result.Status = StatusDegraded
			}
		} else {
			check.Status = StatusReady
			check.Detail = "available on PATH"
		}
		result.Checks = append(result.Checks, check)
	}
	return result
}

func WriteJSON(output io.Writer, result Result) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func WriteText(output io.Writer, result Result) {
	fmt.Fprintf(output, "Cockpit readiness: %s\n\n", strings.ToUpper(result.Status))
	checks := append([]Check(nil), result.Checks...)
	sort.SliceStable(checks, func(i, j int) bool {
		if checks[i].Category == checks[j].Category {
			return checks[i].Name < checks[j].Name
		}
		return checks[i].Category < checks[j].Category
	})
	for _, check := range checks {
		requirement := "optional"
		if check.Required {
			requirement = "required"
		}
		fmt.Fprintf(output, "%-10s %-10s %-8s (%s)  %s\n", check.Category, check.Name, check.Status, requirement, check.Detail)
		if check.Action != "" {
			fmt.Fprintf(output, "  action: %s\n", check.Action)
		}
	}
}
