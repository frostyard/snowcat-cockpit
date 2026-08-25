package nodeup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validImage = "ghcr.io/frostyard/snowcat-cockpit-worker:claude-v0.2.1@sha256:a9dc6cccde4cfdf7dff9788e415ccec87f9a81c394650fc0a84930c527bdbd61"

func testDefaults() Defaults {
	return Defaults{StateDirectory: "/state", ObserverEnv: "/config/profile-observer.env", WorkerEnv: "/config/mcp-token.env"}
}

func validConfigJSON() string {
	return `{
  "version": 1,
  "listen": "127.0.0.1:7686",
  "images": {"codex": "` + validImage + `", "claude": "` + validImage + `", "copilot": "` + validImage + `"},
  "environment": {"CODEX_HOME": "/home/operator/.codex"},
  "providers": {"codex": {"mcpServer": "snowcat"}, "claude": {"mcpServer": "snowcat"}, "copilot": {"mcpServer": "snowcat-mcp"}},
  "repositories": ["frostyard/clix", "frostyard/snowcat"],
  "campaign": {"adapter": "oci", "discoverer": {"provider": "codex", "capacity": 4}, "implementer": {"provider": "claude", "capacity": 4}, "reviewer": {"provider": "copilot", "capacity": 4}}
}`
}

func TestParseAppliesDefaultsAndResolvesLanes(t *testing.T) {
	t.Parallel()

	config, err := Parse([]byte(validConfigJSON()), testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if config.StateDirectory != "/state" || config.ObserverEnv != "/config/profile-observer.env" || config.WorkerEnv != "/config/mcp-token.env" {
		t.Fatalf("defaults not applied: %#v", config)
	}
	if config.SkillsDirectory() != "/state/worker-kit" || config.SourceRoot() != "/state/sources" {
		t.Fatalf("canonical layout = %q %q", config.SkillsDirectory(), config.SourceRoot())
	}
	if config.Campaign.Runtime != "podman" || config.Campaign.IntervalSeconds != 30 {
		t.Fatalf("campaign defaults = %#v", config.Campaign)
	}
	request := config.CampaignRequest()
	if request.Reviewer.MCPServer != "snowcat-mcp" || request.Implementer.MCPServer != "snowcat" || request.Discoverer.Capacity != 4 || request.Runtime != "podman" {
		t.Fatalf("campaign request = %#v", request)
	}
	pairs := config.LanePairs()
	if len(pairs) != 3 || pairs[0] != (LanePair{"codex", "snowcat"}) || pairs[2] != (LanePair{"copilot", "snowcat-mcp"}) {
		t.Fatalf("lane pairs = %#v", pairs)
	}
}

func TestLanePairsDeduplicateSharedProviders(t *testing.T) {
	t.Parallel()

	content := strings.ReplaceAll(validConfigJSON(), `"provider": "codex"`, `"provider": "claude"`)
	content = strings.ReplaceAll(content, `"provider": "copilot"`, `"provider": "claude"`)
	config, err := Parse([]byte(content), testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if pairs := config.LanePairs(); len(pairs) != 1 || pairs[0].Provider != "claude" {
		t.Fatalf("lane pairs = %#v", pairs)
	}
}

func TestServiceEnvironmentProjectsImagesOverAmbientAndDeclaredValues(t *testing.T) {
	t.Parallel()

	config, err := Parse([]byte(validConfigJSON()), testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	ambient := map[string]string{
		"PATH":                             "/usr/bin",
		"CODEX_HOME":                       "/ambient/.codex",
		"SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE": "sha256:" + strings.Repeat("0", 64),
		"GH_TOKEN":                         "must-not-project",
	}
	environment := config.ServiceEnvironment(ambient)
	if environment["PATH"] != "/usr/bin" || environment["CODEX_HOME"] != "/home/operator/.codex" {
		t.Fatalf("declared values must win over ambient: %#v", environment)
	}
	for _, name := range []string{"SNOWCAT_COCKPIT_OCI_CLAUDE_IMAGE", "SNOWCAT_COCKPIT_DOCKER_CLAUDE_IMAGE", "SNOWCAT_COCKPIT_OCI_CODEX_IMAGE", "SNOWCAT_COCKPIT_DOCKER_COPILOT_IMAGE"} {
		if environment[name] != validImage {
			t.Fatalf("%s = %q", name, environment[name])
		}
	}
	if _, leaked := environment["GH_TOKEN"]; leaked {
		t.Fatal("non-allowlisted ambient value projected")
	}
}

func TestParseRejectsInvalidConfigurations(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(string) string
		wantErr string
	}{
		{"unknown field", func(s string) string { return strings.Replace(s, `"listen"`, `"extra": 1, "listen"`, 1) }, "unknown field"},
		{"wrong version", func(s string) string { return strings.Replace(s, `"version": 1`, `"version": 2`, 1) }, "version must be 1"},
		{"non-loopback listen", func(s string) string { return strings.Replace(s, "127.0.0.1:7686", "0.0.0.0:7686", 1) }, "loopback"},
		{"unknown provider", func(s string) string {
			return strings.Replace(s, `"codex": {"mcpServer": "snowcat"}`, `"gemini": {"mcpServer": "snowcat"}`, 1)
		}, "providers.gemini"},
		{"lane names undeclared provider", func(s string) string {
			return strings.Replace(s, `"discoverer": {"provider": "codex"`, `"discoverer": {"provider": "gemini"`, 1)
		}, "campaign.discoverer.provider"},
		{"unpinned image", func(s string) string {
			return strings.Replace(s, validImage+`", "claude"`, `ghcr.io/x:latest", "claude"`, 1)
		}, "images.codex must be pinned"},
		{"image via environment", func(s string) string {
			return strings.Replace(s, `"CODEX_HOME": "/home/operator/.codex"`, `"SNOWCAT_COCKPIT_OCI_CODEX_IMAGE": "`+validImage+`"`, 1)
		}, "must be declared under images"},
		{"non-allowlisted environment", func(s string) string {
			return strings.Replace(s, `"CODEX_HOME": "/home/operator/.codex"`, `"HOME": "/root"`, 1)
		}, "not in the node service allowlist"},
		{"duplicate repository", func(s string) string { return strings.Replace(s, `"frostyard/snowcat"`, `"Frostyard/Clix"`, 1) }, "listed twice"},
		{"malformed repository", func(s string) string { return strings.Replace(s, `"frostyard/snowcat"`, `"snowcat"`, 1) }, "must be owner/name"},
		{"no repositories", func(s string) string {
			return strings.Replace(s, `["frostyard/clix", "frostyard/snowcat"]`, `[]`, 1)
		}, "at least one owner/name"},
		{"capacity over total", func(s string) string {
			return strings.Replace(s, `"capacity": 4}, "reviewer"`, `"capacity": 5}, "reviewer"`, 1)
		}, "may not exceed 12"},
		{"capacity zero", func(s string) string {
			return strings.Replace(s, `"reviewer": {"provider": "copilot", "capacity": 4}`, `"reviewer": {"provider": "copilot", "capacity": 0}`, 1)
		}, "campaign.reviewer.capacity"},
		{"host adapter with runtime", func(s string) string {
			return strings.Replace(s, `"adapter": "oci"`, `"adapter": "host", "runtime": "podman"`, 1)
		}, "runtime is valid only for the oci adapter"},
		{"unknown adapter", func(s string) string { return strings.Replace(s, `"adapter": "oci"`, `"adapter": "vm"`, 1) }, "adapter must be host or oci"},
		{"interval out of range", func(s string) string {
			return strings.Replace(s, `"adapter": "oci"`, `"adapter": "oci", "intervalSeconds": 5`, 1)
		}, "intervalSeconds"},
		{"oci lane without image", func(s string) string {
			return strings.Replace(s, `"copilot": "`+validImage+`"`, `"copilot": "`+validImage+`x"`, 1)
		}, "images.copilot"},
		{"snowcat token", func(s string) string {
			return strings.Replace(s, `"/home/operator/.codex"`, `"snowcat_abcdef123456"`, 1)
		}, "looks like a credential"},
		{"github token in unknown field", func(s string) string {
			return strings.Replace(s, `"listen"`, `"token": "ghp_0123456789abcdef", "listen"`, 1)
		}, "looks like a credential"},
		{"github token in list", func(s string) string {
			return strings.Replace(s, `"frostyard/snowcat"`, `"gho_0123456789abcdef"`, 1)
		}, "looks like a credential"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse([]byte(testCase.mutate(validConfigJSON())), testDefaults())
			if err == nil {
				t.Fatal("expected an error")
			}
			if !errors.Is(err, ErrInvalid) && !strings.Contains(err.Error(), "loopback") {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not mention %q", err, testCase.wantErr)
			}
			if strings.Contains(err.Error(), "ghp_0123456789abcdef") || strings.Contains(err.Error(), "snowcat_abcdef123456") || strings.Contains(err.Error(), "gho_0123456789abcdef") {
				t.Fatalf("error echoes a credential-shaped value: %q", err)
			}
		})
	}
}

func TestHostAdapterNeedsNoImages(t *testing.T) {
	t.Parallel()

	content := strings.Replace(validConfigJSON(), `"adapter": "oci"`, `"adapter": "host"`, 1)
	content = strings.Replace(content, `"images": {"codex": "`+validImage+`", "claude": "`+validImage+`", "copilot": "`+validImage+`"},`, "", 1)
	config, err := Parse([]byte(content), testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if config.Campaign.Runtime != "" || len(config.Images) != 0 {
		t.Fatalf("host config = %#v", config.Campaign)
	}
	if environment := config.ServiceEnvironment(nil); len(environment) != 1 || environment["CODEX_HOME"] == "" {
		t.Fatalf("environment = %#v", environment)
	}
}

func TestLoadReadsRegularFilesOnly(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	path := filepath.Join(directory, "node.json")
	if err := os.WriteFile(path, []byte(validConfigJSON()), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, testDefaults())
	if err != nil {
		t.Fatal(err)
	}
	if config.Listen != "127.0.0.1:7686" || len(config.Repositories) != 2 {
		t.Fatalf("config = %#v", config)
	}
	if _, err := Load(filepath.Join(directory, "missing.json"), testDefaults()); err == nil {
		t.Fatal("expected a missing-file error")
	}
	if _, err := Load(directory, testDefaults()); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("directory error = %v", err)
	}
	if _, err := Load("", testDefaults()); err == nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty path error = %v", err)
	}
}
