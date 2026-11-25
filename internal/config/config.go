package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is the effective configuration after all layers are merged.
type Config struct {
	Version    int              `yaml:"version"`
	Providers  ProvidersConfig  `yaml:"providers"`
	Resolution ResolutionConfig `yaml:"resolution"`
	Skills     SkillsConfig     `yaml:"skills"`
	Policy     PolicyConfig     `yaml:"policy"`
	Execution  ExecutionConfig  `yaml:"execution"`
	Audit      AuditConfig      `yaml:"audit"`
	Daemon     DaemonConfig     `yaml:"daemon"`
	// Locked lists dotted keys that lower layers may not change.
	// It is only read from the system layer.
	Locked []string `yaml:"locked"`
}

// ProvidersConfig holds the preference order, per-provider settings keyed by
// provider id, and custom ACP agents.
type ProvidersConfig struct {
	Order    []string                 `yaml:"order"`
	Custom   []CustomProvider         `yaml:"custom"`
	Settings map[string]ProviderEntry `yaml:",inline"`
}

// ProviderEntry configures one provider.
type ProviderEntry struct {
	Enabled *bool `yaml:"enabled"`
	// Integration selects the protocol where a provider supports several.
	Integration string `yaml:"integration"`
	// Executable pins an absolute path instead of detecting one.
	Executable string `yaml:"executable"`
	// Args replaces the manifest's launch arguments.
	Args []string `yaml:"args"`
	// Model is used when a task does not choose one.
	Model string `yaml:"model"`
	// AllowAccountLogin lets rbridge use a runtime whose terms make API keys
	// the safe default, with the user's own account login instead.
	AllowAccountLogin bool `yaml:"allow_account_login"`
	// Accounts and DiscoverAccounts apply to claude-code only: each extra
	// Claude Code account becomes provider claude-code-<name>.
	Accounts         []AccountConfig `yaml:"accounts"`
	DiscoverAccounts *bool           `yaml:"discover_accounts"`
}

// AccountConfig declares one Claude Code account.
type AccountConfig struct {
	Name string `yaml:"name"`
	// ConfigDir is the account's CLAUDE_CONFIG_DIR; ~ and $HOME are expanded.
	ConfigDir string `yaml:"config_dir"`
	// Launcher is an optional wrapper script that starts this account.
	Launcher string `yaml:"launcher"`
}

// Discovering reports whether accounts are discovered automatically (default on).
func (e ProviderEntry) Discovering() bool { return e.DiscoverAccounts == nil || *e.DiscoverAccounts }

// IsEnabled reports whether the provider is enabled; providers are enabled by default.
func (e ProviderEntry) IsEnabled() bool { return e.Enabled == nil || *e.Enabled }

// CustomProvider declares an ACP agent that rbridge does not ship a manifest for.
type CustomProvider struct {
	ID          string       `yaml:"id"`
	DisplayName string       `yaml:"display_name"`
	Integration string       `yaml:"integration"`
	Launch      LaunchConfig `yaml:"launch"`
	VersionArgs []string     `yaml:"version_args"`
}

// LaunchConfig is how to start a runtime.
type LaunchConfig struct {
	Command        string   `yaml:"command"`
	Args           []string `yaml:"args"`
	EnvPassthrough []string `yaml:"env_passthrough"`
}

// ResolutionConfig is reserved for fallback rules between providers. Neither
// key changes behaviour yet: rbridge picks the first available provider that
// meets the task's requirements, and never replaces a provider the task names.
type ResolutionConfig struct {
	Fallback                string `yaml:"fallback"`
	AllowBillingClassChange bool   `yaml:"allow_billing_class_change"`
}

type SkillsConfig struct {
	ProjectDir               string `yaml:"project_dir"`
	AllowUnreviewed          bool   `yaml:"allow_unreviewed"`
	GitRequirePinnedRevision bool   `yaml:"git_require_pinned_revision"`
}

type PolicyConfig struct {
	DefaultProfile  string        `yaml:"default_profile"`
	ApprovalTimeout time.Duration `yaml:"approval_timeout"`
	EnvAllowlist    []string      `yaml:"env_allowlist"`
	Custom          CustomProfile `yaml:"custom"`
	// RequireEnforced fails a task when one of these controls is wanted by
	// the profile but cannot be enforced (for example network).
	RequireEnforced []string `yaml:"require_enforced"`
}

// CustomProfile configures the "custom" policy profile.
type CustomProfile struct {
	// Rules map ACP tool kinds (read, edit, execute, ...) to allow, deny or ask.
	Rules         map[string]string `yaml:"rules"`
	OutsideRead   string            `yaml:"outside_read"`
	ClientFSWrite bool              `yaml:"client_fs_write"`
	BlockNetwork  bool              `yaml:"block_network"`
}

type ExecutionConfig struct {
	MaxConcurrentSessions int           `yaml:"max_concurrent_sessions"`
	TaskTimeout           time.Duration `yaml:"task_timeout"`
	CommandTimeout        time.Duration `yaml:"command_timeout"`
	MaxOutputBytes        int64         `yaml:"max_output_bytes"`
	KillGracePeriod       time.Duration `yaml:"kill_grace_period"`
	DetectTimeout         time.Duration `yaml:"detect_timeout"`
	IdleTimeout           time.Duration `yaml:"idle_timeout"`
	DetectCacheTTL        time.Duration `yaml:"detect_cache_ttl"`
}

type AuditConfig struct {
	Enabled   bool          `yaml:"enabled"`
	Level     string        `yaml:"level"`
	Retention time.Duration `yaml:"retention"`
}

// DaemonConfig configures `rbridge daemon`.
type DaemonConfig struct {
	HTTP HTTPConfig `yaml:"http"`
	IPC  struct {
		Enabled bool `yaml:"enabled"`
	} `yaml:"ipc"`
	// EventBuffer is how many events per session are kept for replay.
	EventBuffer int `yaml:"event_buffer"`
	// RequestsPerSecond limits each client; bursts of twice the rate are allowed.
	RequestsPerSecond float64 `yaml:"requests_per_second"`
	// SessionIdleTimeout closes daemon sessions nobody has used for this long.
	SessionIdleTimeout time.Duration `yaml:"session_idle_timeout"`
}

// HTTPConfig is the daemon's local HTTP listener.
type HTTPConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"`
	Port    int    `yaml:"port"`
	// AllowedOrigins lists browser origins that may call the API. Empty means
	// requests carrying an Origin header are refused.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// AllowNonLoopback must be set to listen on anything but a loopback address.
	AllowNonLoopback bool `yaml:"allow_non_loopback"`
}

// Defaults returns the built-in configuration, the lowest layer.
func Defaults() Config {
	return Config{
		Version: 1,
		Providers: ProvidersConfig{
			Order:    []string{"claude-code", "opencode"},
			Settings: map[string]ProviderEntry{},
		},
		Resolution: ResolutionConfig{Fallback: "denied"},
		Skills:     SkillsConfig{ProjectDir: ".agents/skills", GitRequirePinnedRevision: true},
		Policy: PolicyConfig{
			DefaultProfile:  "standard",
			ApprovalTimeout: 2 * time.Minute,
			EnvAllowlist:    []string{"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "TERM", "TMPDIR", "XDG_RUNTIME_DIR", "SystemRoot", "ComSpec", "PATHEXT", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "TEMP", "TMP"},
		},
		Execution: ExecutionConfig{
			MaxConcurrentSessions: 2,
			TaskTimeout:           30 * time.Minute,
			CommandTimeout:        10 * time.Minute,
			MaxOutputBytes:        10 << 20,
			KillGracePeriod:       10 * time.Second,
			DetectTimeout:         5 * time.Second,
			IdleTimeout:           5 * time.Minute,
			DetectCacheTTL:        10 * time.Minute,
		},
		Audit: AuditConfig{Enabled: true, Level: "metadata", Retention: 30 * 24 * time.Hour},
		Daemon: func() DaemonConfig {
			d := DaemonConfig{
				HTTP:               HTTPConfig{Host: "127.0.0.1", Port: 7432},
				EventBuffer:        2000,
				RequestsPerSecond:  20,
				SessionIdleTimeout: 30 * time.Minute,
			}
			d.IPC.Enabled = true
			return d
		}(),
	}
}

// Layer names, lowest precedence first.
const (
	LayerDefaults = "defaults"
	LayerSystem   = "system"
	LayerUser     = "user"
	LayerProject  = "project"
	LayerTask     = "task"
)

// LayerInfo describes one layer that took part in a load.
type LayerInfo struct {
	Name    string   `json:"name"`
	Path    string   `json:"path,omitempty"`
	Found   bool     `json:"found"`
	Ignored []string `json:"ignored,omitempty"` // keys dropped because the system layer locked them
}

// LoadOptions select the optional layers.
type LoadOptions struct {
	// ProjectDir, when set, is searched for ProjectFileName.
	ProjectDir string
	// Task holds task-level overrides as a nested map with the same shape as the file format.
	Task map[string]any
}

// Load merges defaults, system, user, project and task layers.
func Load(paths Paths, opts LoadOptions) (Config, []LayerInfo, error) {
	merged, err := toMap(Defaults())
	if err != nil {
		return Config{}, nil, err
	}
	layers := []LayerInfo{{Name: LayerDefaults, Found: true}}

	sys, found, err := readLayer(paths.SystemConfig)
	if err != nil {
		return Config{}, nil, err
	}
	layers = append(layers, LayerInfo{Name: LayerSystem, Path: paths.SystemConfig, Found: found})
	var locked []string
	if found {
		locked = stringList(sys["locked"])
		merge(merged, sys)
	}

	add := func(name, path string, m map[string]any, found bool) {
		info := LayerInfo{Name: name, Path: path, Found: found}
		if found {
			delete(m, "locked")
			info.Ignored = dropLocked(m, locked)
			merge(merged, m)
		}
		layers = append(layers, info)
	}

	user, found, err := readLayer(paths.UserConfigFile())
	if err != nil {
		return Config{}, nil, err
	}
	add(LayerUser, paths.UserConfigFile(), user, found)

	if opts.ProjectDir != "" {
		p := filepath.Join(opts.ProjectDir, ProjectFileName)
		proj, found, err := readLayer(p)
		if err != nil {
			return Config{}, nil, err
		}
		add(LayerProject, p, proj, found)
	}
	if opts.Task != nil {
		task, err := normalize(opts.Task)
		if err != nil {
			return Config{}, nil, fmt.Errorf("task configuration: %w", err)
		}
		add(LayerTask, "", task, true)
	}

	cfg, err := fromMap(merged)
	if err != nil {
		return Config{}, nil, err
	}
	cfg.Locked = locked
	if err := cfg.Validate(); err != nil {
		return Config{}, nil, err
	}
	return cfg, layers, nil
}

// readLayer reads one YAML file. A missing file is not an error.
func readLayer(path string) (map[string]any, bool, error) {
	if path == "" {
		return nil, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read config %s: %w", path, err)
	}
	m := map[string]any{}
	if len(bytes.TrimSpace(b)) == 0 {
		return m, true, nil
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, false, fmt.Errorf("parse config %s: %w", path, err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, true, nil
}

// merge copies src into dst. Nested maps merge; every other value replaces.
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sv, ok := v.(map[string]any); ok {
			if dv, ok := dst[k].(map[string]any); ok {
				merge(dv, sv)
				continue
			}
			cp := map[string]any{}
			merge(cp, sv)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// dropLocked removes locked dotted keys from m and returns the ones it removed.
func dropLocked(m map[string]any, locked []string) []string {
	var removed []string
	for _, key := range locked {
		parts := strings.Split(key, ".")
		cur := m
		for i, p := range parts {
			if i == len(parts)-1 {
				if _, ok := cur[p]; ok {
					delete(cur, p)
					removed = append(removed, key)
				}
				break
			}
			next, ok := cur[p].(map[string]any)
			if !ok {
				break
			}
			cur = next
		}
	}
	return removed
}

func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toMap(c Config) (map[string]any, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, yaml.Unmarshal(b, &m)
}

// normalize round-trips a Go map through YAML so its values have the same
// types as values read from a file.
func normalize(in map[string]any) (map[string]any, error) {
	b, err := yaml.Marshal(in)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, yaml.Unmarshal(b, &m)
}

func fromMap(m map[string]any) (Config, error) {
	b, err := yaml.Marshal(m)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	if c.Providers.Settings == nil {
		c.Providers.Settings = map[string]ProviderEntry{}
	}
	return c, nil
}

var providerIDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidProviderID reports whether id is a lowercase, hyphen-separated identifier.
func ValidProviderID(id string) bool { return len(id) <= 64 && providerIDPattern.MatchString(id) }

var profiles = []string{"observe", "standard", "controlled", "custom"}

// Validate checks values that the YAML decoder cannot.
func (c Config) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported config version %d", c.Version)
	}
	for _, id := range c.Providers.Order {
		if !ValidProviderID(id) {
			return fmt.Errorf("providers.order: invalid provider id %q", id)
		}
	}
	for id, e := range c.Providers.Settings {
		if !ValidProviderID(id) {
			return fmt.Errorf("providers: invalid provider id %q", id)
		}
		if e.Executable != "" && !filepath.IsAbs(e.Executable) {
			return fmt.Errorf("providers.%s.executable must be an absolute path", id)
		}
		if (len(e.Accounts) > 0 || e.DiscoverAccounts != nil) && id != "claude-code" {
			return fmt.Errorf("providers.%s: accounts apply to claude-code only", id)
		}
		for i, a := range e.Accounts {
			if !ValidProviderID(a.Name) || len(a.Name) > 40 {
				return fmt.Errorf("providers.%s.accounts[%d]: invalid name %q", id, i, a.Name)
			}
			if a.ConfigDir == "" {
				return fmt.Errorf("providers.%s.accounts.%s: config_dir is required", id, a.Name)
			}
			if a.Launcher != "" && !filepath.IsAbs(a.Launcher) {
				return fmt.Errorf("providers.%s.accounts.%s: launcher must be an absolute path", id, a.Name)
			}
		}
	}
	seen := map[string]bool{}
	for i, p := range c.Providers.Custom {
		if !ValidProviderID(p.ID) {
			return fmt.Errorf("providers.custom[%d]: invalid id %q", i, p.ID)
		}
		if seen[p.ID] {
			return fmt.Errorf("providers.custom: duplicate id %q", p.ID)
		}
		seen[p.ID] = true
		if p.Integration != "" && p.Integration != "acp" {
			return fmt.Errorf("providers.custom.%s: integration must be acp", p.ID)
		}
		if p.Launch.Command == "" {
			return fmt.Errorf("providers.custom.%s: launch.command is required", p.ID)
		}
	}
	if !slices.Contains(profiles, c.Policy.DefaultProfile) {
		return fmt.Errorf("policy.default_profile must be one of %v", profiles)
	}
	if c.Resolution.Fallback != "denied" && c.Resolution.Fallback != "allowed" {
		return errors.New("resolution.fallback must be denied or allowed")
	}
	switch c.Audit.Level {
	case "metadata", "events", "full":
	default:
		return errors.New("audit.level must be metadata, events or full")
	}
	if c.Execution.MaxConcurrentSessions < 1 {
		return errors.New("execution.max_concurrent_sessions must be at least 1")
	}
	for name, d := range map[string]time.Duration{
		"execution.task_timeout":      c.Execution.TaskTimeout,
		"execution.command_timeout":   c.Execution.CommandTimeout,
		"execution.kill_grace_period": c.Execution.KillGracePeriod,
		"execution.detect_timeout":    c.Execution.DetectTimeout,
		"execution.idle_timeout":      c.Execution.IdleTimeout,
		"policy.approval_timeout":     c.Policy.ApprovalTimeout,
	} {
		if d <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if c.Execution.DetectCacheTTL < 0 {
		return errors.New("execution.detect_cache_ttl must not be negative")
	}
	if c.Execution.MaxOutputBytes <= 0 {
		return errors.New("execution.max_output_bytes must be positive")
	}
	if c.Daemon.HTTP.Port < 0 || c.Daemon.HTTP.Port > 65535 {
		return errors.New("daemon.http.port must be 0 to 65535")
	}
	if c.Daemon.EventBuffer < 10 {
		return errors.New("daemon.event_buffer must be at least 10")
	}
	if c.Daemon.RequestsPerSecond <= 0 {
		return errors.New("daemon.requests_per_second must be positive")
	}
	if c.Skills.ProjectDir == "" || filepath.IsAbs(c.Skills.ProjectDir) {
		return errors.New("skills.project_dir must be a relative path")
	}
	return nil
}
