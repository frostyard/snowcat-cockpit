package profile

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/frostyard/snowcat-cockpit/internal/queueview"
)

const maxSkillBytes int64 = 1 << 20

const (
	StatusReady             = "ready"
	StatusMissing           = "missing"
	StatusDrifted           = "drifted"
	StatusUnchecked         = "unchecked"
	StatusFailed            = "failed"
	StatusExpired           = "expired"
	StatusPreflightRequired = "preflight-required"
)

//go:embed worker-kit.lock.json
var manifestJSON []byte

//go:embed worker-kit/*/SKILL.md
var workerKit embed.FS

type Source struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type LockedSkill struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version int           `json:"version"`
	Source  Source        `json:"source"`
	Skills  []LockedSkill `json:"skills"`
}

type Check struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	Action string `json:"action,omitempty"`
}

type SkillCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type Kit struct {
	Status   string       `json:"status"`
	Source   Source       `json:"source"`
	Version  int          `json:"version"`
	Checks   []SkillCheck `json:"checks"`
	Detail   string       `json:"detail"`
	Revision string       `json:"revision"`
}

type Role struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Skill       string   `json:"skill"`
	Selection   string   `json:"selection"`
	ExactKinds  []string `json:"exactKinds,omitempty"`
	KindSuffix  string   `json:"kindSuffix,omitempty"`
	Description string   `json:"description"`
}

type Provider struct {
	ID         string   `json:"id"`
	Label      string   `json:"label"`
	Executable Check    `json:"executable"`
	SkillKit   Check    `json:"skillKit"`
	MCP        Check    `json:"mcp"`
	Roles      []string `json:"roles"`
	Status     string   `json:"status"`
}

type Snapshot struct {
	Status    string     `json:"status"`
	Kit       Kit        `json:"kit"`
	Roles     []Role     `json:"roles"`
	Providers []Provider `json:"providers"`
}

type PreflightReceipt struct {
	Status      string
	Detail      string
	MCPServer   string
	CheckedAt   time.Time
	ExpiresAt   time.Time
	KitRevision string
}

type InstallResult struct {
	Directory string       `json:"directory"`
	Status    string       `json:"status"`
	Checks    []SkillCheck `json:"checks"`
}

type providerDefinition struct {
	id         string
	label      string
	executable string
}

var providerDefinitions = []providerDefinition{
	{id: "codex", label: "Codex", executable: "codex"},
	{id: "claude", label: "Claude", executable: "claude"},
	{id: "copilot", label: "Copilot", executable: "copilot"},
}

var roles = buildRoles()

func buildRoles() []Role {
	discoverer, _ := queueview.ClassificationRuleFor(queueview.RoleDiscoverer)
	implementer, _ := queueview.ClassificationRuleFor(queueview.RoleImplementer)
	reviewer, _ := queueview.ClassificationRuleFor(queueview.RoleReviewer)
	return []Role{
		{
			ID:          string(discoverer.Role),
			Label:       "Discoverer",
			Skill:       "work-snowcat-queue",
			Selection:   queueview.RoleSelection(discoverer.Role),
			KindSuffix:  discoverer.Suffix,
			Description: "Claims one read-only discovery item and proposes at most one bounded child for operator admission.",
		},
		{
			ID:          string(implementer.Role),
			Label:       "Implementer",
			Skill:       "work-snowcat-queue",
			Selection:   queueview.RoleSelection(implementer.Role),
			Description: "Claims one bounded worker delivery item and delivers only within its authority.",
		},
		{
			ID:          string(reviewer.Role),
			Label:       "Reviewer",
			Skill:       "review-snowcat-queue",
			Selection:   queueview.RoleSelection(reviewer.Role),
			ExactKinds:  append([]string(nil), reviewer.ExactKinds...),
			Description: "Claims one independent pull-request review and reports a structured verdict.",
		},
	}
}

func Inspect(skillsDirectory string) Snapshot {
	return inspectWithPreflights(mustManifest(), skillsDirectory, exec.LookPath, nil, time.Now().UTC())
}

func InspectWithPreflights(skillsDirectory string, receipts map[string]PreflightReceipt, now time.Time) Snapshot {
	return inspectWithPreflights(mustManifest(), skillsDirectory, exec.LookPath, receipts, now)
}

func LockedManifest() Manifest {
	return mustManifest()
}

// InstallKit materializes the embedded, locked worker kit without replacing
// any existing file. A conflicting file is reported as drift and left intact.
func InstallKit(skillsDirectory string) (InstallResult, error) {
	result := InstallResult{Directory: skillsDirectory, Status: StatusReady}
	if skillsDirectory == "" {
		return result, fmt.Errorf("worker kit directory is not configured")
	}

	manifest := mustManifest()
	type payload struct {
		locked LockedSkill
		bytes  []byte
		path   string
	}
	payloads := make([]payload, 0, len(manifest.Skills))
	for _, skill := range manifest.Skills {
		content, err := workerKit.ReadFile(filepath.ToSlash(filepath.Join("worker-kit", skill.Name, "SKILL.md")))
		if err != nil {
			return result, fmt.Errorf("read embedded skill %s: %w", skill.Name, err)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != skill.SHA256 {
			return result, fmt.Errorf("embedded skill %s does not match worker kit lock", skill.Name)
		}
		payloads = append(payloads, payload{
			locked: skill,
			bytes:  content,
			path:   filepath.Join(skillsDirectory, skill.Name, "SKILL.md"),
		})
	}

	// Refuse the whole operation before writing if any existing skill drifts.
	for _, item := range payloads {
		digest, err := fileDigest(item.path)
		if errorsIsNotExist(err) {
			continue
		}
		if err != nil {
			return result, fmt.Errorf("inspect existing skill %s: %w", item.locked.Name, err)
		}
		if digest != item.locked.SHA256 {
			result.Status = StatusDrifted
			result.Checks = append(result.Checks, SkillCheck{
				Name: item.locked.Name, Status: StatusDrifted,
				Detail: "existing file differs; left unchanged",
			})
			return result, fmt.Errorf("refusing to replace drifted skill %s", item.locked.Name)
		}
	}

	if err := os.MkdirAll(skillsDirectory, 0o700); err != nil {
		return result, fmt.Errorf("create worker kit directory: %w", err)
	}
	for _, item := range payloads {
		if digest, err := fileDigest(item.path); err == nil && digest == item.locked.SHA256 {
			result.Checks = append(result.Checks, SkillCheck{
				Name: item.locked.Name, Status: StatusReady, Detail: "already matches the locked revision",
			})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(item.path), 0o700); err != nil {
			return result, fmt.Errorf("create skill directory %s: %w", item.locked.Name, err)
		}
		file, err := os.OpenFile(item.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return result, fmt.Errorf("create skill %s: %w", item.locked.Name, err)
		}
		writeErr := func() error {
			if _, err := file.Write(item.bytes); err != nil {
				return err
			}
			return file.Sync()
		}()
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(item.path)
			if writeErr != nil {
				return result, fmt.Errorf("write skill %s: %w", item.locked.Name, writeErr)
			}
			return result, fmt.Errorf("close skill %s: %w", item.locked.Name, closeErr)
		}
		result.Checks = append(result.Checks, SkillCheck{
			Name: item.locked.Name, Status: StatusReady, Detail: "installed from the locked worker kit",
		})
	}
	return result, nil
}

func WriteJSON(output io.Writer, snapshot Snapshot) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}

func WriteInstallJSON(output io.Writer, result InstallResult) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}

func WriteInstallText(output io.Writer, result InstallResult) {
	fmt.Fprintf(output, "Snowcat worker kit: %s\n", result.Status)
	fmt.Fprintf(output, "Directory: %s\n", result.Directory)
	for _, check := range result.Checks {
		fmt.Fprintf(output, "  %-30s %-8s %s\n", check.Name, check.Status, check.Detail)
	}
}

func WriteText(output io.Writer, snapshot Snapshot) {
	fmt.Fprintf(output, "Worker profiles: %s\n", snapshot.Status)
	fmt.Fprintf(output, "Snowcat worker kit: %s (%s)\n\n", snapshot.Kit.Status, shortRevision(snapshot.Kit.Revision))
	for _, provider := range snapshot.Providers {
		fmt.Fprintf(output, "%-9s %-18s executable=%-8s kit=%-8s mcp=%s\n",
			provider.Label, provider.Status, provider.Executable.Status, provider.SkillKit.Status, provider.MCP.Status)
	}
	fmt.Fprintln(output, "\nRoles:")
	for _, role := range snapshot.Roles {
		fmt.Fprintf(output, "  %-12s %-26s %s\n", role.ID, role.Skill, role.Selection)
	}
}

func inspect(manifest Manifest, skillsDirectory string, lookPath func(string) (string, error)) Snapshot {
	return inspectWithPreflights(manifest, skillsDirectory, lookPath, nil, time.Now().UTC())
}

func inspectWithPreflights(manifest Manifest, skillsDirectory string, lookPath func(string) (string, error), receipts map[string]PreflightReceipt, now time.Time) Snapshot {
	kit := inspectKit(manifest, skillsDirectory)
	providers := make([]Provider, 0, len(providerDefinitions))
	overallStatus := StatusReady
	for _, definition := range providerDefinitions {
		executable := Check{Status: StatusReady, Detail: "available on PATH"}
		if _, err := lookPath(definition.executable); err != nil {
			executable = Check{
				Status: StatusMissing,
				Detail: "not found on PATH",
				Action: fmt.Sprintf("Install %s before launching this provider.", definition.label),
			}
		}
		skillKit := Check{Status: kit.Status, Detail: kit.Detail}
		if kit.Status != StatusReady {
			skillKit.Action = "Install the locked Snowcat worker kit, then check profiles again."
		}
		mcp, status := preflightCheck(receipts[definition.id], kit.Revision, now)
		if executable.Status != StatusReady || skillKit.Status != StatusReady {
			status = StatusMissing
		}
		providers = append(providers, Provider{
			ID:         definition.id,
			Label:      definition.label,
			Executable: executable,
			SkillKit:   skillKit,
			MCP:        mcp,
			Roles:      []string{"discoverer", "implementer", "reviewer"},
			Status:     status,
		})
		overallStatus = combineStatus(overallStatus, status)
	}
	return Snapshot{
		Status:    overallStatus,
		Kit:       kit,
		Roles:     append([]Role(nil), roles...),
		Providers: providers,
	}
}

func preflightCheck(receipt PreflightReceipt, kitRevision string, now time.Time) (Check, string) {
	if receipt.Status == "" {
		return Check{
			Status: StatusUnchecked,
			Detail: "live Snowcat MCP smoke test has not run",
			Action: "Run the explicit provider preflight before launch.",
		}, StatusPreflightRequired
	}
	if receipt.KitRevision != kitRevision {
		return Check{
			Status: StatusExpired,
			Detail: "preflight used a different worker kit revision",
			Action: "Run the provider preflight against the current kit.",
		}, StatusPreflightRequired
	}
	if receipt.Status == StatusFailed {
		return Check{
			Status: StatusFailed,
			Detail: receipt.Detail,
			Action: "Correct provider auth or MCP configuration, then run preflight again.",
		}, StatusFailed
	}
	if receipt.Status != StatusReady || !receipt.ExpiresAt.After(now) {
		return Check{
			Status: StatusExpired,
			Detail: "live Snowcat MCP preflight has expired",
			Action: "Run the provider preflight again before launch.",
		}, StatusPreflightRequired
	}
	return Check{
		Status: StatusReady,
		Detail: fmt.Sprintf("%s list_work proved at %s", receipt.MCPServer, receipt.CheckedAt.UTC().Format(time.RFC3339)),
	}, StatusReady
}

func combineStatus(current, provider string) string {
	priority := map[string]int{
		StatusReady:             0,
		StatusPreflightRequired: 1,
		StatusFailed:            2,
		StatusMissing:           3,
	}
	if priority[provider] > priority[current] {
		return provider
	}
	return current
}

func inspectKit(manifest Manifest, skillsDirectory string) Kit {
	kit := Kit{
		Status:   StatusReady,
		Source:   manifest.Source,
		Version:  manifest.Version,
		Revision: manifest.Source.Revision,
		Checks:   make([]SkillCheck, 0, len(manifest.Skills)),
	}
	if skillsDirectory == "" {
		kit.Status = StatusMissing
		kit.Detail = "worker kit directory is not configured"
		for _, skill := range manifest.Skills {
			kit.Checks = append(kit.Checks, SkillCheck{Name: skill.Name, Status: StatusMissing, Detail: "not checked"})
		}
		return kit
	}

	for _, skill := range manifest.Skills {
		path := filepath.Join(skillsDirectory, skill.Name, "SKILL.md")
		digest, err := fileDigest(path)
		if err != nil {
			kit.Status = StatusMissing
			detail := "locked skill is absent"
			if !errorsIsNotExist(err) {
				detail = "locked skill cannot be verified"
			}
			kit.Checks = append(kit.Checks, SkillCheck{Name: skill.Name, Status: StatusMissing, Detail: detail})
			continue
		}
		if digest != skill.SHA256 {
			if kit.Status != StatusMissing {
				kit.Status = StatusDrifted
			}
			kit.Checks = append(kit.Checks, SkillCheck{Name: skill.Name, Status: StatusDrifted, Detail: "content differs from the locked revision"})
			continue
		}
		kit.Checks = append(kit.Checks, SkillCheck{Name: skill.Name, Status: StatusReady, Detail: "matches the locked revision"})
	}

	switch kit.Status {
	case StatusReady:
		kit.Detail = "all canonical Snowcat skills match the locked revision"
	case StatusDrifted:
		kit.Detail = "one or more Snowcat skills differ from the locked revision"
	case StatusMissing:
		kit.Detail = "one or more locked Snowcat skills are absent"
	}
	return kit
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("not a regular file")
	}
	if info.Size() > maxSkillBytes {
		return "", fmt.Errorf("file exceeds %d bytes", maxSkillBytes)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func errorsIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}

func mustManifest() Manifest {
	var manifest Manifest
	if err := json.Unmarshal(manifestJSON, &manifest); err != nil {
		panic(fmt.Sprintf("decode embedded worker kit manifest: %v", err))
	}
	return manifest
}

func shortRevision(revision string) string {
	if len(revision) <= 12 {
		return revision
	}
	return revision[:12]
}
