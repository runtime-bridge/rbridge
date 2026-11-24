// Package registry holds provider manifests: the built-in ones and custom ACP
// agents declared in configuration.
package registry

import (
	"fmt"
	"slices"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
)

// Manifest describes how to find, launch and use one runtime.
type Manifest struct {
	ID          string
	DisplayName string
	Class       domain.ProviderClass
	Integration domain.Integration
	BuiltIn     bool

	Detect DetectSpec
	Launch LaunchSpec
	Skills SkillDirs

	Capabilities domain.CapabilitySet
	// AuthEnv names environment variables whose presence means the runtime
	// will use API-key or cloud-provider billing. Values are never read.
	AuthEnv []string
	// RequireAuthEnv makes detection report needs-api-key unless one of
	// AuthEnv is set, or the user allows account login for this provider.
	RequireAuthEnv bool
	// AccountLoginForbidden ignores allow_account_login.
	AccountLoginForbidden bool
	Limits                []string
	Notes                 []string
}

// DetectSpec says where to look for the runtime and how to ask its version.
type DetectSpec struct {
	Executables []string
	// KnownDirs are searched after PATH. They may start with ~ or $VAR.
	KnownDirs   []string
	VersionArgs []string
}

// LaunchSpec is how the runtime is started for a session.
type LaunchSpec struct {
	Args []string
	// EnvPassthrough names variables passed to the runtime in addition to
	// the policy allowlist.
	EnvPassthrough []string
}

// SkillDirs are the runtime's native skill discovery locations. Entries may
// start with ~ or $VAR; project entries are relative to the workspace.
// "a|b" means the first alternative that resolves, for runtimes where an
// environment variable replaces the default location.
type SkillDirs struct {
	Project []string
	User    []string
	// SessionDir is a layout rbridge can create for one session and hand to
	// the runtime, relative to that directory. Empty when unsupported.
	SessionDir string
	// Strategy is the per-session delivery method (see package deliver).
	// Empty means the runtime cannot be given skills for one session.
	Strategy string
}

// caps sets the source on each entry; capabilities not listed are unknown
// until a handshake reports them.
func caps(src domain.CapabilitySource, entries map[domain.CapabilityName]domain.Capability) domain.CapabilitySet {
	out := make(domain.CapabilitySet, len(domain.KnownCapabilities))
	for _, n := range domain.KnownCapabilities {
		out[n] = domain.Capability{Supported: domain.SupportUnknown, Source: src}
	}
	for k, v := range entries {
		v.Source = src
		out[k] = v
	}
	return out
}

func yes(mode string) domain.Capability {
	return domain.Capability{Supported: domain.SupportYes, Mode: mode}
}

func no() domain.Capability { return domain.Capability{Supported: domain.SupportNo} }

func partial(mode string) domain.Capability {
	return domain.Capability{Supported: domain.SupportPartial, Mode: mode}
}

func unknown() domain.Capability { return domain.Capability{Supported: domain.SupportUnknown} }

var commonUnixDirs = []string{"~/.local/bin", "/usr/local/bin", "/opt/homebrew/bin", "~/.npm-global/bin", "~/bin"}

// ClaudeCode is the built-in manifest for Claude Code driven in headless mode.
// Capability values were confirmed against Claude Code 2.1.331 in spike S3 and S4.
func ClaudeCode() Manifest {
	return Manifest{
		ID:          "claude-code",
		DisplayName: "Claude Code",
		Class:       domain.ClassAgentRuntime,
		Integration: domain.IntegrationClaudeHeadless,
		BuiltIn:     true,
		Detect: DetectSpec{
			Executables: []string{"claude"},
			KnownDirs:   append([]string{"~/.claude/local", "$APPDATA/npm"}, commonUnixDirs...),
			VersionArgs: []string{"--version"},
		},
		Launch: LaunchSpec{
			Args:           []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
			EnvPassthrough: []string{"CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"},
		},
		Skills: SkillDirs{
			Project:    []string{".claude/skills"},
			User:       []string{"$CLAUDE_CONFIG_DIR/skills|~/.claude/skills"},
			SessionDir: ".claude/skills",
			Strategy:   "session-dir",
		},
		Capabilities: caps(domain.SourceManifest, map[domain.CapabilityName]domain.Capability{
			domain.CapStreaming:          yes("stream-json"),
			domain.CapSessionsPersistent: yes("stream-json input"),
			domain.CapSessionsResume:     yes("--resume"),
			domain.CapSessionsLoad:       no(),
			domain.CapCancel:             yes("interrupt control request"),
			domain.CapSkills:             yes("native discovery, session directory via --add-dir"),
			domain.CapMCPStdio:           yes("--mcp-config"),
			domain.CapMCPHTTP:            yes("--mcp-config"),
			domain.CapFilesystem:         yes("built-in tools"),
			domain.CapShell:              yes("built-in tools"),
			domain.CapStructuredOutput:   yes("--json-schema"),
			domain.CapApprovals:          yes("host permission prompts"),
			domain.CapPromptImage:        unknown(),
			domain.CapPromptAudio:        unknown(),
			domain.CapUsage:              yes("result usage"),
			domain.CapOffline:            no(),
		}),
		AuthEnv: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"},
		Limits: []string{
			"File and shell access use Claude Code's own tools; rbridge policy applies through its permission flags.",
			"Does not read .agents/skills; skills are delivered to .claude/skills locations.",
		},
		Notes: []string{"Sign-in is checked by Claude Code itself; rbridge only sees the result of a run."},
	}
}

// OpenCode is the built-in manifest for OpenCode over ACP.
// Capability values come from the ACP handshake of OpenCode 1.19.6 in spike S1.
func OpenCode() Manifest {
	return Manifest{
		ID:          "opencode",
		DisplayName: "OpenCode",
		Class:       domain.ClassAgentRuntime,
		Integration: domain.IntegrationACP,
		BuiltIn:     true,
		Detect: DetectSpec{
			Executables: []string{"opencode"},
			KnownDirs:   append([]string{"~/.opencode/bin", "$APPDATA/npm"}, commonUnixDirs...),
			VersionArgs: []string{"--version"},
		},
		Launch: LaunchSpec{
			Args:           []string{"acp"},
			EnvPassthrough: []string{"OLLAMA_HOST", "OPENCODE_CONFIG", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "XDG_STATE_HOME"},
		},
		Skills: SkillDirs{
			Project:  []string{".opencode/skills", ".agents/skills", ".claude/skills"},
			User:     []string{"~/.config/opencode/skills", "~/.agents/skills", "~/.claude/skills"},
			Strategy: "opencode-config",
		},
		Capabilities: caps(domain.SourceManifest, map[domain.CapabilityName]domain.Capability{
			domain.CapStreaming:          yes("acp session/update"),
			domain.CapSessionsPersistent: yes(""),
			domain.CapSessionsLoad:       yes("session/load"),
			domain.CapSessionsResume:     yes("session/resume"),
			domain.CapCancel:             yes("session/cancel"),
			domain.CapSkills:             yes("native discovery"),
			domain.CapMCPStdio:           yes(""),
			domain.CapMCPHTTP:            yes(""),
			domain.CapFilesystem:         yes("built-in tools or client fs"),
			domain.CapShell:              yes("built-in tools"),
			domain.CapStructuredOutput:   unknown(),
			domain.CapApprovals:          yes("session/request_permission"),
			domain.CapPromptImage:        yes(""),
			domain.CapPromptAudio:        no(),
			domain.CapUsage:              unknown(),
			domain.CapOffline:            partial("with a local model provider such as Ollama"),
		}),
		Limits: []string{
			"One prompt at a time per session.",
			"The session working directory must be absolute.",
			"MCP servers over ACP are not supported.",
		},
		Notes: []string{"Uses the model providers configured in OpenCode, for example a local Ollama server."},
	}
}

// GeminiCLI is the built-in manifest for Gemini CLI over ACP. Its terms
// forbid driving the services behind Gemini CLI's Google login from
// third-party software, so an API key is required unless the user opts in.
func GeminiCLI() Manifest {
	return Manifest{
		ID:          "gemini-cli",
		DisplayName: "Gemini CLI",
		Class:       domain.ClassAgentRuntime,
		Integration: domain.IntegrationACP,
		BuiltIn:     true,
		Detect: DetectSpec{
			Executables: []string{"gemini"},
			KnownDirs:   append([]string{"$APPDATA/npm"}, commonUnixDirs...),
			VersionArgs: []string{"--version"},
		},
		Launch: LaunchSpec{
			Args:           []string{"--acp"},
			EnvPassthrough: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_APPLICATION_CREDENTIALS", "GEMINI_MODEL"},
		},
		Skills: SkillDirs{
			Project: []string{".gemini/skills", ".agents/skills"},
			User:    []string{"~/.gemini/skills", "~/.agents/skills"},
		},
		Capabilities: caps(domain.SourceManifest, map[domain.CapabilityName]domain.Capability{
			domain.CapStreaming: yes("acp session/update"),
			domain.CapCancel:    yes("session/cancel"),
			domain.CapSkills:    partial("native discovery only; no per-session delivery yet"),
			domain.CapApprovals: yes("session/request_permission"),
			domain.CapOffline:   no(),
		}),
		AuthEnv:        []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI"},
		RequireAuthEnv: true,
		Limits: []string{
			"Needs GEMINI_API_KEY, GOOGLE_API_KEY or Vertex AI settings unless providers.gemini-cli.allow_account_login is set.",
			"Older releases use --experimental-acp; set providers.gemini-cli.args if yours does.",
		},
		Notes: []string{"Gemini CLI's terms forbid using its Google login from third-party software; read them before allowing account login."},
	}
}

// Codex is the built-in manifest for OpenAI Codex through the codex-acp adapter.
func Codex() Manifest {
	return Manifest{
		ID:          "codex",
		DisplayName: "Codex",
		Class:       domain.ClassAgentRuntime,
		Integration: domain.IntegrationACP,
		BuiltIn:     true,
		Detect: DetectSpec{
			Executables: []string{"codex-acp"},
			KnownDirs:   append([]string{"$APPDATA/npm"}, commonUnixDirs...),
			VersionArgs: []string{"--version"},
		},
		Launch: LaunchSpec{
			EnvPassthrough: []string{"CODEX_API_KEY", "OPENAI_API_KEY", "CODEX_HOME", "NO_BROWSER"},
		},
		Skills: SkillDirs{
			Project: []string{".agents/skills"},
		},
		Capabilities: caps(domain.SourceManifest, map[domain.CapabilityName]domain.Capability{
			domain.CapStreaming: yes("acp session/update"),
			domain.CapCancel:    yes("session/cancel"),
			domain.CapSkills:    partial("native discovery only; no per-session delivery yet"),
			domain.CapApprovals: yes("session/request_permission"),
			domain.CapOffline:   no(),
		}),
		AuthEnv: []string{"CODEX_API_KEY", "OPENAI_API_KEY"},
		Notes:   []string{"OpenAI recommends API keys (CODEX_API_KEY) for automated use; a ChatGPT login also works with codex-acp."},
	}
}

// ClaudeACP is Claude through the ACP adapter, which is built on the Claude
// Agent SDK. Anthropic's terms require API key authentication for that, so
// account login cannot be enabled (decision D10).
func ClaudeACP() Manifest {
	return Manifest{
		ID:          "claude-acp",
		DisplayName: "Claude (ACP adapter)",
		Class:       domain.ClassAgentRuntime,
		Integration: domain.IntegrationACP,
		BuiltIn:     true,
		Detect: DetectSpec{
			Executables: []string{"claude-agent-acp"},
			KnownDirs:   append([]string{"$APPDATA/npm"}, commonUnixDirs...),
			VersionArgs: []string{"--version"},
		},
		Launch: LaunchSpec{
			EnvPassthrough: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_EXECUTABLE", "CLAUDE_CONFIG_DIR", "AWS_REGION", "AWS_PROFILE", "CLOUD_ML_REGION", "ANTHROPIC_VERTEX_PROJECT_ID"},
		},
		Skills: SkillDirs{
			Project: []string{".claude/skills"},
			User:    []string{"$CLAUDE_CONFIG_DIR/skills|~/.claude/skills"},
		},
		Capabilities: caps(domain.SourceManifest, map[domain.CapabilityName]domain.Capability{
			domain.CapStreaming: yes("acp session/update"),
			domain.CapCancel:    yes("session/cancel"),
			domain.CapSkills:    partial("native discovery only; use claude-code for per-session delivery"),
			domain.CapApprovals: yes("session/request_permission"),
			domain.CapOffline:   no(),
		}),
		AuthEnv:               []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"},
		RequireAuthEnv:        true,
		AccountLoginForbidden: true,
		Limits:                []string{"Runs only with an Anthropic API key or a cloud provider; use claude-code for your own Claude subscription."},
	}
}

// BuiltIns returns every built-in manifest.
func BuiltIns() []Manifest {
	return []Manifest{ClaudeCode(), OpenCode(), Codex(), GeminiCLI(), ClaudeACP()}
}

// FromCustom turns a configured custom ACP agent into a manifest. Its
// capabilities are unknown until a handshake reports them.
func FromCustom(c config.CustomProvider) Manifest {
	name := c.DisplayName
	if name == "" {
		name = c.ID
	}
	m := Manifest{
		ID:          c.ID,
		DisplayName: name,
		Class:       domain.ClassCustom,
		Integration: domain.IntegrationACP,
		Detect: DetectSpec{
			Executables: []string{c.Launch.Command},
			VersionArgs: c.VersionArgs,
		},
		Launch: LaunchSpec{
			Args:           c.Launch.Args,
			EnvPassthrough: c.Launch.EnvPassthrough,
		},
		Capabilities: domain.CapabilitySet{},
	}
	return m
}

// Registry is the ordered set of known providers.
type Registry struct {
	manifests []Manifest
	byID      map[string]int
}

// New builds the registry from the built-ins and the configuration.
// Providers listed in providers.order come first, in that order.
func New(cfg config.Config) (*Registry, error) {
	all := BuiltIns()
	for _, c := range cfg.Providers.Custom {
		all = append(all, FromCustom(c))
	}
	r := &Registry{byID: map[string]int{}}
	for _, m := range all {
		if _, dup := r.byID[m.ID]; dup {
			return nil, fmt.Errorf("provider id %q is already used by a built-in provider", m.ID)
		}
		r.byID[m.ID] = len(r.manifests)
		r.manifests = append(r.manifests, m)
	}
	for id := range cfg.Providers.Settings {
		if _, ok := r.byID[id]; !ok {
			return nil, fmt.Errorf("providers.%s: no such provider", id)
		}
	}
	rank := map[string]int{}
	for i, id := range cfg.Providers.Order {
		if _, ok := r.byID[id]; !ok {
			return nil, fmt.Errorf("providers.order: no such provider %q", id)
		}
		if _, seen := rank[id]; !seen {
			rank[id] = i
		}
	}
	slices.SortStableFunc(r.manifests, func(a, b Manifest) int {
		ra, aok := rank[a.ID]
		rb, bok := rank[b.ID]
		switch {
		case aok && bok:
			return ra - rb
		case aok:
			return -1
		case bok:
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	for i, m := range r.manifests {
		r.byID[m.ID] = i
	}
	return r, nil
}

// All returns the manifests in preference order.
func (r *Registry) All() []Manifest { return slices.Clone(r.manifests) }

// Get returns one manifest.
func (r *Registry) Get(id string) (Manifest, bool) {
	i, ok := r.byID[id]
	if !ok {
		return Manifest{}, false
	}
	return r.manifests[i], true
}
