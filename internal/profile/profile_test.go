package profile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/queueview"
)

func TestInstallKitMaterializesEmbeddedSkillsAndIsIdempotent(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "skills")
	first, err := InstallKit(root)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != StatusReady || len(first.Checks) != len(LockedManifest().Skills) {
		t.Fatalf("first install = %#v", first)
	}
	for _, skill := range LockedManifest().Skills {
		path := filepath.Join(root, skill.Name, "SKILL.md")
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", skill.Name, info.Mode().Perm())
		}
	}

	second, err := InstallKit(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range second.Checks {
		if check.Detail != "already matches the locked revision" {
			t.Fatalf("second install check = %#v", check)
		}
	}

	snapshot := inspect(LockedManifest(), root, func(name string) (string, error) {
		return "/test/bin/" + name, nil
	})
	if snapshot.Kit.Status != StatusReady {
		t.Fatalf("installed kit status = %q", snapshot.Kit.Status)
	}
	if len(snapshot.Roles) != 3 || snapshot.Roles[0].ID != "discoverer" || snapshot.Roles[0].KindSuffix != "-discovery" {
		t.Fatalf("roles = %#v", snapshot.Roles)
	}
	if snapshot.Roles[1].Selection != queueview.RoleSelection(queueview.RoleImplementer) {
		t.Fatalf("implementer selection = %q", snapshot.Roles[1].Selection)
	}
	if len(snapshot.Roles[2].ExactKinds) != 1 || snapshot.Roles[2].ExactKinds[0] != "pr-review" {
		t.Fatalf("reviewer kinds = %#v", snapshot.Roles[2].ExactKinds)
	}
}

func TestInstallKitRefusesToReplaceDrift(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "skills")
	if _, err := InstallKit(root); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, LockedManifest().Skills[0].Name, "SKILL.md")
	drift := []byte("operator-owned drift\n")
	if err := os.WriteFile(path, drift, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := InstallKit(root)
	if err == nil {
		t.Fatal("InstallKit succeeded over a drifted file")
	}
	if result.Status != StatusDrifted {
		t.Fatalf("status = %q, want drifted", result.Status)
	}
	content, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(content, drift) {
		t.Fatalf("drifted file was changed: %q", content)
	}
}

func TestInspectReportsReadyKitAndPreflightRequired(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := []byte("locked skill\n")
	digest := sha256.Sum256(content)
	manifest := Manifest{
		Version: 1,
		Source:  Source{Repository: "https://example.test/snowcat", Revision: "abc123"},
		Skills:  []LockedSkill{{Name: "work", SHA256: hex.EncodeToString(digest[:])}},
	}
	writeSkill(t, root, "work", content)
	snapshot := inspect(manifest, root, func(name string) (string, error) {
		return "/test/bin/" + name, nil
	})
	if snapshot.Kit.Status != StatusReady {
		t.Fatalf("kit status = %q, want ready", snapshot.Kit.Status)
	}
	if snapshot.Status != StatusPreflightRequired {
		t.Fatalf("snapshot status = %q, want preflight-required", snapshot.Status)
	}
	for _, provider := range snapshot.Providers {
		if provider.Status != StatusPreflightRequired {
			t.Fatalf("%s status = %q", provider.ID, provider.Status)
		}
		if provider.MCP.Status != StatusUnchecked {
			t.Fatalf("%s MCP status = %q", provider.ID, provider.MCP.Status)
		}
	}
}

func TestInspectAppliesCurrentPreflightReceipts(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := []byte("locked skill\n")
	digest := sha256.Sum256(content)
	manifest := Manifest{
		Version: 1,
		Source:  Source{Repository: "https://example.test/snowcat", Revision: "abc123"},
		Skills:  []LockedSkill{{Name: "work", SHA256: hex.EncodeToString(digest[:])}},
	}
	writeSkill(t, root, "work", content)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	receipts := make(map[string]PreflightReceipt)
	for _, provider := range providerDefinitions {
		receipts[provider.id] = PreflightReceipt{
			Status: StatusReady, MCPServer: "snowcat", CheckedAt: now, ExpiresAt: now.Add(15 * time.Minute), KitRevision: "abc123",
		}
	}
	snapshot := inspectWithPreflights(manifest, root, func(name string) (string, error) {
		return "/test/bin/" + name, nil
	}, receipts, now)
	if snapshot.Status != StatusReady {
		t.Fatalf("snapshot status = %q, want ready", snapshot.Status)
	}
	for _, provider := range snapshot.Providers {
		if provider.Status != StatusReady || provider.MCP.Status != StatusReady {
			t.Fatalf("provider = %#v", provider)
		}
		if len(provider.Roles) != 3 || provider.Roles[0] != "discoverer" {
			t.Fatalf("provider roles = %#v", provider.Roles)
		}
	}
}

func TestInspectRejectsExpiredAndFailedPreflights(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	content := []byte("locked skill\n")
	digest := sha256.Sum256(content)
	manifest := Manifest{
		Version: 1,
		Source:  Source{Repository: "https://example.test/snowcat", Revision: "abc123"},
		Skills:  []LockedSkill{{Name: "work", SHA256: hex.EncodeToString(digest[:])}},
	}
	writeSkill(t, root, "work", content)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	receipts := map[string]PreflightReceipt{
		"codex":  {Status: StatusReady, CheckedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute), KitRevision: "abc123"},
		"claude": {Status: StatusFailed, Detail: "provider exited without proof", CheckedAt: now, ExpiresAt: now, KitRevision: "abc123"},
	}
	snapshot := inspectWithPreflights(manifest, root, func(name string) (string, error) {
		return "/test/bin/" + name, nil
	}, receipts, now)
	if snapshot.Status != StatusFailed {
		t.Fatalf("snapshot status = %q, want failed", snapshot.Status)
	}
	if snapshot.Providers[0].MCP.Status != StatusExpired {
		t.Fatalf("codex MCP = %#v", snapshot.Providers[0].MCP)
	}
	if snapshot.Providers[1].MCP.Status != StatusFailed {
		t.Fatalf("claude MCP = %#v", snapshot.Providers[1].MCP)
	}
}

func TestInspectReportsDriftAndMissingExecutable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	manifest := Manifest{
		Version: 1,
		Source:  Source{Repository: "https://example.test/snowcat", Revision: "abc123"},
		Skills:  []LockedSkill{{Name: "work", SHA256: "0000000000000000000000000000000000000000000000000000000000000000"}},
	}
	writeSkill(t, root, "work", []byte("different\n"))
	snapshot := inspect(manifest, root, func(string) (string, error) {
		return "", errors.New("missing")
	})
	if snapshot.Kit.Status != StatusDrifted {
		t.Fatalf("kit status = %q, want drifted", snapshot.Kit.Status)
	}
	if snapshot.Status != StatusMissing {
		t.Fatalf("snapshot status = %q, want missing", snapshot.Status)
	}
	for _, provider := range snapshot.Providers {
		if provider.Executable.Status != StatusMissing {
			t.Fatalf("%s executable status = %q", provider.ID, provider.Executable.Status)
		}
	}
}

func TestInspectReportsMissingKitWithoutReadingCurrentDirectory(t *testing.T) {
	t.Parallel()

	manifest := Manifest{
		Version: 1,
		Source:  Source{Repository: "https://example.test/snowcat", Revision: "abc123"},
		Skills:  []LockedSkill{{Name: "work", SHA256: "unused"}},
	}
	snapshot := inspect(manifest, "", func(name string) (string, error) {
		return "/test/bin/" + name, nil
	})
	if snapshot.Kit.Status != StatusMissing {
		t.Fatalf("kit status = %q, want missing", snapshot.Kit.Status)
	}
}

func writeSkill(t *testing.T, root, name string, content []byte) {
	t.Helper()
	directory := filepath.Join(root, name)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), content, 0o600); err != nil {
		t.Fatal(err)
	}
}
