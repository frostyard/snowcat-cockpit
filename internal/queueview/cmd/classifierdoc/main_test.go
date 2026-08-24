package main

import (
	"os"
	"testing"
)

func TestWorkerProfilesContainsGeneratedClassifierTable(t *testing.T) {
	t.Parallel()
	path := "../../../../docs/specs/worker-profiles.md"
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := replaceGeneratedTable(string(content))
	if err != nil {
		t.Fatal(err)
	}
	if updated != string(content) {
		t.Fatalf("%s classifier table is stale; run go generate ./internal/queueview", path)
	}
}
