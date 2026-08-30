package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

const (
	preflightDirectory      = "preflights"
	preflightVersion        = 1
	maxPreflightReceiptSize = 16 * 1024
)

var (
	providerPattern = regexp.MustCompile(`^(codex|claude|copilot)$`)
	revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type PreflightReceipt struct {
	Version     int       `json:"version"`
	Provider    string    `json:"provider"`
	MCPServer   string    `json:"mcpServer"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail"`
	CheckedAt   time.Time `json:"checkedAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
	KitRevision string    `json:"kitRevision"`
}

func WritePreflight(directory string, receipt PreflightReceipt) error {
	if _, err := Open(directory); err != nil {
		return err
	}
	receipt.Version = preflightVersion
	if err := validatePreflight(receipt); err != nil {
		return err
	}

	preflights := filepath.Join(directory, preflightDirectory)
	if err := os.MkdirAll(preflights, 0o700); err != nil {
		return fmt.Errorf("create preflight state directory: %w", err)
	}
	if err := os.Chmod(preflights, 0o700); err != nil {
		return fmt.Errorf("secure preflight state directory: %w", err)
	}
	path := filepath.Join(preflights, receipt.Provider+".json")
	temporary, err := os.CreateTemp(preflights, ".preflight-*.json")
	if err != nil {
		return fmt.Errorf("create temporary preflight receipt: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure temporary preflight receipt: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(receipt); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode preflight receipt: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync preflight receipt: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close preflight receipt: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("install preflight receipt: %w", err)
	}
	return nil
}

func ReadPreflights(directory string) (map[string]PreflightReceipt, error) {
	result := make(map[string]PreflightReceipt)
	entries, err := os.ReadDir(filepath.Join(directory, preflightDirectory))
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read preflight state directory: %w", err)
	}
	if len(entries) > 16 {
		return nil, errors.New("too many preflight state entries")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || entry.Name()[0] == '.' {
			continue
		}
		path := filepath.Join(directory, preflightDirectory, entry.Name())
		receipt, err := readPreflight(path)
		if err != nil {
			return nil, err
		}
		if entry.Name() != receipt.Provider+".json" {
			return nil, fmt.Errorf("preflight receipt filename does not match provider")
		}
		result[receipt.Provider] = receipt
	}
	return result, nil
}

func readPreflight(path string) (PreflightReceipt, error) {
	info, err := os.Stat(path)
	if err != nil {
		return PreflightReceipt{}, err
	}
	if info.Size() > maxPreflightReceiptSize {
		return PreflightReceipt{}, fmt.Errorf("preflight receipt exceeds %d byte limit", maxPreflightReceiptSize)
	}
	file, err := os.Open(path)
	if err != nil {
		return PreflightReceipt{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var receipt PreflightReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return PreflightReceipt{}, fmt.Errorf("decode preflight receipt: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return PreflightReceipt{}, errors.New("preflight receipt contains trailing content")
		}
		return PreflightReceipt{}, fmt.Errorf("decode preflight receipt: %w", err)
	}
	if err := validatePreflight(receipt); err != nil {
		return PreflightReceipt{}, err
	}
	return receipt, nil
}

func validatePreflight(receipt PreflightReceipt) error {
	if receipt.Version != preflightVersion {
		return fmt.Errorf("invalid preflight receipt version")
	}
	if !providerPattern.MatchString(receipt.Provider) {
		return fmt.Errorf("invalid preflight provider")
	}
	if receipt.Status != "ready" && receipt.Status != "failed" {
		return fmt.Errorf("invalid preflight status")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(receipt.MCPServer) {
		return fmt.Errorf("invalid preflight MCP server")
	}
	if receipt.Detail == "" || len(receipt.Detail) > 200 {
		return fmt.Errorf("invalid preflight detail")
	}
	if receipt.CheckedAt.IsZero() || receipt.ExpiresAt.Before(receipt.CheckedAt) {
		return fmt.Errorf("invalid preflight timestamps")
	}
	if !revisionPattern.MatchString(receipt.KitRevision) {
		return fmt.Errorf("invalid preflight kit revision")
	}
	return nil
}
