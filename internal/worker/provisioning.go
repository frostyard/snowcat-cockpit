package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Provisioning records the repository tools an OCI worker was given at target
// preparation (ADR-0012 §2–3): the digest of the repository's pin files, the
// read-only cache the container mounts, and the tools the cache holds.
type Provisioning struct {
	LockDigest    string    `json:"lockDigest"`
	Cache         string    `json:"cache"`
	Tools         []string  `json:"tools"`
	ProvisionedAt time.Time `json:"provisionedAt"`
}

// MiseDataDirectory is where the base image expects the provisioned tool
// cache (oci/Containerfile ENV MISE_DATA_DIR); it is mounted read-only.
const MiseDataDirectory = "/var/lib/snowcat-cockpit/mise"

// provisionScript runs mise inside a throwaway container of the worker image
// so the cache is built by the image's own mise and architecture. It works
// from a tmpfs copy of the three pin files: the workspace stays read-only and
// is never written, and --locked refuses any tool the lock does not
// pre-resolve (core ADR-0043; ADR-0023 checksums come from the lock).
const provisionScript = `set -eu
mkdir -p /tmp/provision
cp /workspace/mise.toml /workspace/mise.lock /tmp/provision/
[ -f /workspace/go.mod ] && cp /workspace/go.mod /tmp/provision/ || true
cd /tmp/provision
mise install --locked
mise reshim
missing="$(mise ls --missing 2>/dev/null || true)"
if [ -n "$missing" ]; then printf 'tools still missing after install:\n%s\n' "$missing" >&2; exit 1; fi
mise ls --installed --json`

var pinFiles = []string{"mise.toml", "mise.lock", "go.mod"}

// provisionTools provisions the repository's declared tools for an OCI worker
// before its lease exists. A workspace without mise.toml provisions nothing
// and is not unready for it. A repository whose declaration cannot be
// satisfied fails the launch with the tool or reason named.
func (manager *Manager) provisionTools(ctx context.Context, record Record, selection ociRuntimeSelection, environment []string) (*Provisioning, error) {
	if _, err := os.Stat(filepath.Join(record.Workspace, "mise.toml")); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect repository pin files: %w", err)
	}
	if _, err := os.Stat(filepath.Join(record.Workspace, "mise.lock")); err != nil {
		return nil, fmt.Errorf("%w: mise.toml without mise.lock — commit the lock mise install produces", ErrNotReady)
	}
	digest, err := pinDigest(record.Workspace)
	if err != nil {
		return nil, err
	}
	owner, name, _ := strings.Cut(record.Repository, "/")
	cache := filepath.Join(manager.stateDirectory, "mise", owner, name, digest)
	marker := filepath.Join(cache, ".provisioned.json")
	if content, err := os.ReadFile(marker); err == nil {
		var previous Provisioning
		if json.Unmarshal(content, &previous) == nil && previous.LockDigest == digest {
			previous.Cache = cache
			return &previous, nil
		}
	}
	if err := os.RemoveAll(cache); err != nil {
		return nil, fmt.Errorf("reset tool cache: %w", err)
	}
	if err := os.MkdirAll(cache, 0o700); err != nil {
		return nil, fmt.Errorf("create tool cache: %w", err)
	}
	arguments := manager.provisionArguments(record, selection.Image, cache)
	output, err := manager.run(ctx, selection.Path, "", environment, arguments...)
	if err != nil {
		_ = os.RemoveAll(cache)
		return nil, fmt.Errorf("%w: %s", ErrNotReady, provisionReason(output))
	}
	tools := installedTools(output)
	result := &Provisioning{LockDigest: digest, Cache: cache, Tools: tools, ProvisionedAt: manager.now().UTC()}
	content, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(marker, append(content, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("record tool cache: %w", err)
	}
	return result, nil
}

// provisionArguments is the worker container's isolation posture with the
// workspace read-only, the cache read-write, no provider inputs, and no
// credentials: provisioning needs only the pin files and the network.
func (manager *Manager) provisionArguments(record Record, image, cache string) []string {
	arguments := []string{
		"run", "--rm", "--pull=never",
		"--name", manager.containerName(record.ID) + "-provision",
		"--read-only",
	}
	if record.Runtime == RuntimePodman {
		arguments = append(arguments,
			"--read-only-tmpfs=false",
			"--userns=keep-id:uid=1000,gid=1000",
			"--user=1000:1000",
			"--cap-drop=ALL", "--security-opt=no-new-privileges",
		)
	} else {
		arguments = append(arguments,
			"--user=1000:1000",
			"--cap-drop=ALL", "--security-opt=no-new-privileges=true",
		)
		if manager.oci.DockerAddHost != "" {
			arguments = append(arguments, "--add-host", manager.oci.DockerAddHost)
		}
	}
	return append(arguments,
		"--cpus=4", "--pids-limit=1024", "--ulimit=core=0:0", "--log-driver=none",
		"--tmpfs=/home/cockpit:rw,size=2g,mode=1777",
		"--tmpfs=/tmp:rw,exec,size=2g,mode=1777",
		"--tmpfs=/var/lib:rw,size=512m,mode=1777",
		"--mount", "type=bind,source="+record.Workspace+",destination=/workspace,readonly",
		"--mount", "type=bind,source="+cache+",destination="+MiseDataDirectory,
		"--env", "MISE_LOCKED=1",
		"--entrypoint", "/bin/sh",
		image, "-c", provisionScript,
	)
}

func pinDigest(workspace string) (string, error) {
	hash := sha256.New()
	for _, name := range pinFiles {
		content, err := os.ReadFile(filepath.Join(workspace, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("read %s: %w", name, err)
		}
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(content))
		hash.Write(content)
	}
	return hex.EncodeToString(hash.Sum(nil))[:32], nil
}

// installedTools reads `mise ls --installed --json`, the provisioning
// script's last output, into sorted "tool@version" entries.
func installedTools(output []byte) []string {
	start := strings.LastIndex(string(output), "\n{")
	if start < 0 {
		start = strings.Index(string(output), "{")
		if start < 0 {
			return nil
		}
	} else {
		start++
	}
	var listing map[string][]struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(output[start:], &listing) != nil {
		return nil
	}
	tools := make([]string, 0, len(listing))
	for tool, versions := range listing {
		for _, version := range versions {
			tools = append(tools, tool+"@"+version.Version)
		}
	}
	sort.Strings(tools)
	return tools
}

// provisionReason keeps the last few non-empty lines of mise's output — the
// line naming the tool, checksum, or lock entry that failed — and nothing
// that could carry a credential (provisioning receives none).
func provisionReason(output []byte) string {
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	kept := make([]string, 0, 3)
	for index := len(lines) - 1; index >= 0 && len(kept) < 3; index-- {
		line := strings.TrimSpace(lines[index])
		if line == "" || strings.HasPrefix(line, "mise ERROR Version:") || strings.HasPrefix(line, "mise ERROR Run with") {
			continue
		}
		kept = append([]string{line}, kept...)
	}
	if len(kept) == 0 {
		return "repository tool provisioning failed"
	}
	return "repository tool provisioning failed: " + strings.Join(kept, " | ")
}
