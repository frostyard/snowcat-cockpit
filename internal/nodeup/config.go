// Package nodeup converges a Linux host to a declared, non-secret Cockpit
// node configuration: doctor, worker kit, systemd user service, managed
// repositories, provider preflights, and the board campaign, in that order and
// idempotently (ADR-0013).
package nodeup

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/frostyard/snowcat-cockpit/internal/campaign"
	"github.com/frostyard/snowcat-cockpit/internal/nodeservice"
	"github.com/frostyard/snowcat-cockpit/internal/worker"
)

const (
	// ConfigVersion is the only node configuration schema this package reads.
	ConfigVersion = 1

	maxConfigBytes  = 256 * 1024
	maxRepositories = 64
	maxLaneCapacity = 12
	minInterval     = 10
	maxInterval     = 300
	defaultInterval = 30
)

var (
	// ErrInvalid marks a configuration that cannot be converged.
	ErrInvalid = errors.New("invalid node configuration")

	providerIDs  = []string{"codex", "claude", "copilot"}
	nameRE       = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	repositoryRE = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
	// tokenRE recognises credential shapes that must never appear in the
	// configuration: Snowcat tokens, GitHub OAuth/PAT/fine-grained tokens, and
	// generic API-key prefixes.
	tokenRE = regexp.MustCompile(`^(snowcat_|gho_|ghp_|ghu_|ghs_|ghr_|github_pat_|sk-)`)
)

// Provider declares the provider-local MCP server name a lane uses.
type Provider struct {
	MCPServer string `json:"mcpServer"`
}

// Lane names a provider and its global capacity; the MCP server comes from
// the provider table so one provider names exactly one server per campaign.
type Lane struct {
	Provider string `json:"provider"`
	Capacity int    `json:"capacity"`
}

// Campaign is the declared board campaign.
type Campaign struct {
	Adapter         string `json:"adapter"`
	Runtime         string `json:"runtime,omitempty"`
	IntervalSeconds int    `json:"intervalSeconds,omitempty"`
	Discoverer      Lane   `json:"discoverer"`
	Implementer     Lane   `json:"implementer"`
	Reviewer        Lane   `json:"reviewer"`
}

// Config is the declared, non-secret node configuration (schema version 1).
type Config struct {
	Version        int                 `json:"version"`
	Listen         string              `json:"listen"`
	StateDirectory string              `json:"stateDirectory,omitempty"`
	ObserverEnv    string              `json:"observerEnv,omitempty"`
	WorkerEnv      string              `json:"workerEnv,omitempty"`
	Images         map[string]string   `json:"images,omitempty"`
	Environment    map[string]string   `json:"environment,omitempty"`
	Providers      map[string]Provider `json:"providers"`
	Repositories   []string            `json:"repositories"`
	Campaign       Campaign            `json:"campaign"`
}

// Defaults fills configuration fields the operator may omit.
type Defaults struct {
	StateDirectory string
	ObserverEnv    string
	WorkerEnv      string
}

// SkillsDirectory is the canonical worker-kit root beneath the state directory.
func (config Config) SkillsDirectory() string {
	return filepath.Join(config.StateDirectory, "worker-kit")
}

// SourceRoot is the canonical managed-source root beneath the state directory.
func (config Config) SourceRoot() string {
	return filepath.Join(config.StateDirectory, "sources")
}

// LanePairs returns the distinct provider/MCP-server pairs the campaign lanes
// use, in lane order (discoverer, implementer, reviewer).
func (config Config) LanePairs() []LanePair {
	var pairs []LanePair
	seen := make(map[string]bool)
	for _, lane := range []Lane{config.Campaign.Discoverer, config.Campaign.Implementer, config.Campaign.Reviewer} {
		if seen[lane.Provider] {
			continue
		}
		seen[lane.Provider] = true
		pairs = append(pairs, LanePair{Provider: lane.Provider, MCPServer: config.Providers[lane.Provider].MCPServer})
	}
	return pairs
}

// LanePair is one provider and the MCP server it proves against.
type LanePair struct {
	Provider  string
	MCPServer string
}

// CampaignRequest resolves the declared lanes into the node API request shape.
func (config Config) CampaignRequest() campaign.Request {
	lane := func(declared Lane) campaign.Lane {
		return campaign.Lane{Provider: declared.Provider, MCPServer: config.Providers[declared.Provider].MCPServer, Capacity: declared.Capacity}
	}
	return campaign.Request{
		Adapter: config.Campaign.Adapter, Runtime: config.Campaign.Runtime, IntervalSeconds: config.Campaign.IntervalSeconds,
		Discoverer: lane(config.Campaign.Discoverer), Implementer: lane(config.Campaign.Implementer), Reviewer: lane(config.Campaign.Reviewer),
	}
}

// ServiceEnvironment merges the ambient allowlisted values, the declared
// environment, and the image pins (which always win and are projected to both
// the Podman and Docker variable names).
func (config Config) ServiceEnvironment(ambient map[string]string) map[string]string {
	merged := make(map[string]string)
	for _, name := range nodeservice.EnvironmentAllowlist {
		if value := ambient[name]; value != "" {
			merged[name] = value
		}
	}
	for name, value := range config.Environment {
		merged[name] = value
	}
	for _, provider := range providerIDs {
		image := config.Images[provider]
		if image == "" {
			continue
		}
		upper := strings.ToUpper(provider)
		merged["SNOWCAT_COCKPIT_OCI_"+upper+"_IMAGE"] = image
		merged["SNOWCAT_COCKPIT_DOCKER_"+upper+"_IMAGE"] = image
	}
	return merged
}

// Load reads, defaults, and validates a configuration file.
func Load(path string, defaults Defaults) (Config, error) {
	if path == "" {
		return Config{}, fmt.Errorf("%w: config path is required", ErrInvalid)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return Config{}, fmt.Errorf("read node configuration: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("%w: config must be a regular file", ErrInvalid)
	}
	if info.Size() > maxConfigBytes {
		return Config{}, fmt.Errorf("%w: config exceeds %d bytes", ErrInvalid, maxConfigBytes)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read node configuration: %w", err)
	}
	return Parse(content, defaults)
}

// Parse decodes and validates configuration bytes.
func Parse(content []byte, defaults Defaults) (Config, error) {
	if err := rejectTokens(content); err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("%w: decode: %v", ErrInvalid, err)
	}
	if decoder.More() {
		return Config{}, fmt.Errorf("%w: trailing content after the configuration object", ErrInvalid)
	}
	if err := config.applyDefaults(defaults); err != nil {
		return Config{}, err
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

// rejectTokens scans every JSON string value before typed decoding so a
// credential pasted into any field — known or unknown — is refused without
// being echoed.
func rejectTokens(content []byte) error {
	var generic any
	if err := json.Unmarshal(content, &generic); err != nil {
		return fmt.Errorf("%w: decode: %v", ErrInvalid, err)
	}
	var walk func(path string, value any) error
	walk = func(path string, value any) error {
		switch typed := value.(type) {
		case string:
			if tokenRE.MatchString(strings.TrimSpace(typed)) {
				return fmt.Errorf("%w: %s looks like a credential; the configuration holds only paths and image references", ErrInvalid, path)
			}
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if err := walk(path+"."+key, typed[key]); err != nil {
					return err
				}
			}
		case []any:
			for index, item := range typed {
				if err := walk(fmt.Sprintf("%s[%d]", path, index), item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk("config", generic)
}

func (config *Config) applyDefaults(defaults Defaults) error {
	if config.StateDirectory == "" {
		config.StateDirectory = defaults.StateDirectory
	}
	if config.ObserverEnv == "" {
		config.ObserverEnv = defaults.ObserverEnv
	}
	if config.WorkerEnv == "" {
		config.WorkerEnv = defaults.WorkerEnv
	}
	if config.Campaign.IntervalSeconds == 0 {
		config.Campaign.IntervalSeconds = defaultInterval
	}
	if config.Campaign.Adapter == worker.AdapterOCI && config.Campaign.Runtime == "" {
		config.Campaign.Runtime = worker.RuntimePodman
	}
	var err error
	for _, target := range []struct {
		value *string
		name  string
	}{
		{&config.StateDirectory, "stateDirectory"}, {&config.ObserverEnv, "observerEnv"}, {&config.WorkerEnv, "workerEnv"},
	} {
		if *target.value == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalid, target.name)
		}
		if *target.value, err = filepath.Abs(*target.value); err != nil {
			return fmt.Errorf("%w: resolve %s", ErrInvalid, target.name)
		}
		*target.value = filepath.Clean(*target.value)
	}
	return nil
}

func (config Config) validate() error {
	if config.Version != ConfigVersion {
		return fmt.Errorf("%w: version must be %d", ErrInvalid, ConfigVersion)
	}
	if err := nodeservice.ValidateListen(config.Listen); err != nil {
		return err
	}
	if len(config.Providers) == 0 {
		return fmt.Errorf("%w: providers must declare at least one provider", ErrInvalid)
	}
	for id, provider := range config.Providers {
		if !isProvider(id) {
			return fmt.Errorf("%w: providers.%s is not codex, claude, or copilot", ErrInvalid, id)
		}
		if !nameRE.MatchString(provider.MCPServer) {
			return fmt.Errorf("%w: providers.%s.mcpServer must be a provider-local MCP server name", ErrInvalid, id)
		}
	}
	for id, image := range config.Images {
		if !isProvider(id) {
			return fmt.Errorf("%w: images.%s is not codex, claude, or copilot", ErrInvalid, id)
		}
		if !worker.PinnedImageRE.MatchString(image) {
			return fmt.Errorf("%w: images.%s must be pinned by sha256 digest", ErrInvalid, id)
		}
	}
	for name, value := range config.Environment {
		if !isAllowlisted(name) {
			return fmt.Errorf("%w: environment.%s is not in the node service allowlist", ErrInvalid, name)
		}
		if strings.HasSuffix(name, "_IMAGE") {
			return fmt.Errorf("%w: environment.%s must be declared under images", ErrInvalid, name)
		}
		if value == "" || strings.ContainsAny(value, "\n\r\x00") {
			return fmt.Errorf("%w: environment.%s must be a single non-empty line", ErrInvalid, name)
		}
	}
	if len(config.Repositories) == 0 {
		return fmt.Errorf("%w: repositories must list at least one owner/name", ErrInvalid)
	}
	if len(config.Repositories) > maxRepositories {
		return fmt.Errorf("%w: repositories may list at most %d entries", ErrInvalid, maxRepositories)
	}
	seen := make(map[string]bool, len(config.Repositories))
	for _, repository := range config.Repositories {
		if !repositoryRE.MatchString(repository) {
			return fmt.Errorf("%w: repository %q must be owner/name", ErrInvalid, repository)
		}
		key := strings.ToLower(repository)
		if seen[key] {
			return fmt.Errorf("%w: repository %s is listed twice", ErrInvalid, repository)
		}
		seen[key] = true
	}
	return config.validateCampaign()
}

func (config Config) validateCampaign() error {
	declared := config.Campaign
	switch declared.Adapter {
	case worker.AdapterHost:
		if declared.Runtime != "" {
			return fmt.Errorf("%w: campaign.runtime is valid only for the oci adapter", ErrInvalid)
		}
	case worker.AdapterOCI:
		if declared.Runtime != worker.RuntimePodman && declared.Runtime != worker.RuntimeDocker {
			return fmt.Errorf("%w: campaign.runtime must be podman or docker", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: campaign.adapter must be host or oci", ErrInvalid)
	}
	if declared.IntervalSeconds < minInterval || declared.IntervalSeconds > maxInterval {
		return fmt.Errorf("%w: campaign.intervalSeconds must be between %d and %d", ErrInvalid, minInterval, maxInterval)
	}
	total := 0
	for _, lane := range []struct {
		name string
		lane Lane
	}{{"discoverer", declared.Discoverer}, {"implementer", declared.Implementer}, {"reviewer", declared.Reviewer}} {
		if _, ok := config.Providers[lane.lane.Provider]; !ok {
			return fmt.Errorf("%w: campaign.%s.provider %q is not declared under providers", ErrInvalid, lane.name, lane.lane.Provider)
		}
		if lane.lane.Capacity < 1 || lane.lane.Capacity > maxLaneCapacity {
			return fmt.Errorf("%w: campaign.%s.capacity must be between 1 and %d", ErrInvalid, lane.name, maxLaneCapacity)
		}
		if declared.Adapter == worker.AdapterOCI && config.Images[lane.lane.Provider] == "" {
			return fmt.Errorf("%w: images.%s is required for the oci adapter", ErrInvalid, lane.lane.Provider)
		}
		total += lane.lane.Capacity
	}
	if total > maxLaneCapacity {
		return fmt.Errorf("%w: campaign lane capacities may not exceed %d in total", ErrInvalid, maxLaneCapacity)
	}
	return nil
}

func isProvider(id string) bool {
	for _, candidate := range providerIDs {
		if candidate == id {
			return true
		}
	}
	return false
}

func isAllowlisted(name string) bool {
	for _, candidate := range nodeservice.EnvironmentAllowlist {
		if candidate == name {
			return true
		}
	}
	return false
}
