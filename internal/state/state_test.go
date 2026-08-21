package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenCreatesAndReusesSecureNodeState(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "cockpit")
	first, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeID != second.NodeID {
		t.Fatalf("node ID changed: %q != %q", first.NodeID, second.NodeID)
	}
	if !nodeIDPattern.MatchString(first.NodeID) {
		t.Fatalf("unexpected node ID %q", first.NodeID)
	}

	directoryInfo, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := directoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o, want 700", got)
	}
	stateInfo, err := os.Stat(filepath.Join(directory, stateFilename))
	if err != nil {
		t.Fatal(err)
	}
	if got := stateInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}
}

func TestWriteAndReadPreflightReceipt(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "cockpit")
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	receipt := PreflightReceipt{
		Provider:    "codex",
		MCPServer:   "snowcat",
		Status:      "ready",
		Detail:      "locked skills visible; Snowcat list_work succeeded",
		CheckedAt:   now,
		ExpiresAt:   now.Add(15 * time.Minute),
		KitRevision: "94852fd3c90e4dbd8560be291695bf14ac90c530",
	}
	if err := WritePreflight(directory, receipt); err != nil {
		t.Fatal(err)
	}
	receipts, err := ReadPreflights(directory)
	if err != nil {
		t.Fatal(err)
	}
	got := receipts["codex"]
	if got.Provider != receipt.Provider || got.Status != receipt.Status || !got.ExpiresAt.Equal(receipt.ExpiresAt) {
		t.Fatalf("receipt = %#v", got)
	}
	info, err := os.Stat(filepath.Join(directory, preflightDirectory, "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("receipt mode = %o, want 600", info.Mode().Perm())
	}
}

func TestReadPreflightsDoesNotCreateState(t *testing.T) {
	t.Parallel()

	directory := filepath.Join(t.TempDir(), "absent")
	receipts, err := ReadPreflights(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(receipts) != 0 {
		t.Fatalf("receipts = %#v", receipts)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("read created state directory: %v", err)
	}
}

func TestOpenRejectsCorruptState(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, stateFilename), []byte(`{"nodeId":"wrong"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(directory); err == nil {
		t.Fatal("expected corrupt state to fail")
	}
}
