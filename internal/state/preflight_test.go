package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validPreflightJSON = `{
  "version": 1,
  "provider": "codex",
  "mcpServer": "snowcat",
  "status": "ready",
  "detail": "locked skills visible; Snowcat list_work succeeded",
  "checkedAt": "2026-08-21T12:00:00Z",
  "expiresAt": "2026-08-21T12:15:00Z",
  "kitRevision": "94852fd3c90e4dbd8560be291695bf14ac90c530"
}`

func writeRawPreflight(t *testing.T, directory, contents string) {
	t.Helper()
	preflights := filepath.Join(directory, preflightDirectory)
	if err := os.MkdirAll(preflights, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preflights, "codex.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadPreflightsRejectsTrailingGarbage(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeRawPreflight(t, directory, validPreflightJSON+"garbage")
	if _, err := ReadPreflights(directory); err == nil {
		t.Fatal("expected trailing garbage to be rejected")
	}
}

func TestReadPreflightsRejectsSecondJSONValue(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeRawPreflight(t, directory, validPreflightJSON+"\n"+validPreflightJSON)
	if _, err := ReadPreflights(directory); err == nil {
		t.Fatal("expected a second JSON value to be rejected")
	}
}

func TestReadPreflightsRejectsOversizedFileWithValidLeadingReceipt(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	padding := strings.Repeat(" ", maxPreflightReceiptSize)
	writeRawPreflight(t, directory, validPreflightJSON+padding)
	if _, err := ReadPreflights(directory); err == nil {
		t.Fatal("expected oversized preflight receipt to be rejected")
	}
}

func TestReadPreflightsAcceptsValidReceipt(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeRawPreflight(t, directory, validPreflightJSON)
	receipts, err := ReadPreflights(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := receipts["codex"]
	if !ok {
		t.Fatalf("receipts = %#v", receipts)
	}
	if got.Status != "ready" || got.MCPServer != "snowcat" {
		t.Fatalf("receipt = %#v", got)
	}
}
