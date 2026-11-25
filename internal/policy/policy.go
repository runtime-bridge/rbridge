// Package policy decides what a runtime may do in a session. Decisions are
// made per action from the ACP tool kind and any paths involved; "ask" hands
// the request to the caller.
package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

// Action is a policy outcome.
type Action string

const (
	Allow Action = "allow"
	Deny  Action = "deny"
	Ask   Action = "ask"
)

// Kinds are the ACP tool kinds; Claude Code tools are mapped onto them.
var Kinds = []string{"read", "search", "think", "fetch", "edit", "delete", "move", "execute", "switch_mode", "other"}

// Profile is a named set of rules.
type Profile struct {
	Name  string
	Rules map[string]Action
	// OutsideRead applies to reads of paths outside the workspace and the
	// session's skill folders. Writes outside the workspace are always denied.
	OutsideRead Action
	// ClientFSWrite says whether ACP agents are offered fs/write_text_file.
	ClientFSWrite bool
	// ClaudeDisallowed are Claude Code tools removed with --disallowed-tools,
	// which Claude Code itself enforces.
	ClaudeDisallowed []string
	// Network says whether the profile wants network access blocked. rbridge
	// cannot enforce that without an OS sandbox.
	BlockNetwork bool
}

func rules(def Action, overrides map[string]Action) map[string]Action {
	r := map[string]Action{}
	for _, k := range Kinds {
		r[k] = def
	}
	maps.Copy(r, overrides)
	return r
}

var readOnly = map[string]Action{"read": Allow, "search": Allow, "think": Allow}

// Builtin returns one of the built-in profiles.
func Builtin(name string) (Profile, bool) {
	switch name {
	case "observe":
		return Profile{
			Name: "observe", Rules: rules(Deny, readOnly), OutsideRead: Deny,
			ClaudeDisallowed: []string{"Write", "Edit", "MultiEdit", "NotebookEdit", "Bash", "WebFetch", "WebSearch"},
			BlockNetwork:     true,
		}, true
	case "standard":
		return Profile{
			Name: "standard", Rules: rules(Ask, map[string]Action{"read": Allow, "search": Allow, "think": Allow, "edit": Allow, "move": Allow}),
			OutsideRead: Ask, ClientFSWrite: true,
		}, true
	case "controlled":
		return Profile{Name: "controlled", Rules: rules(Ask, readOnly), OutsideRead: Ask, ClientFSWrite: true}, true
	}
	return Profile{}, false
}

// Custom builds the custom profile from configuration values.
func Custom(ruleNames map[string]string, outsideRead string, clientWrite, blockNetwork bool) (Profile, error) {
	p := Profile{Name: "custom", Rules: rules(Ask, nil), OutsideRead: Ask, ClientFSWrite: clientWrite, BlockNetwork: blockNetwork}
	for k, v := range ruleNames {
		if !slices.Contains(Kinds, k) {
			return p, fmt.Errorf("policy.custom.rules: unknown kind %q", k)
		}
		a, err := parseAction(v)
		if err != nil {
			return p, fmt.Errorf("policy.custom.rules.%s: %w", k, err)
		}
		p.Rules[k] = a
	}
	if outsideRead != "" {
		a, err := parseAction(outsideRead)
		if err != nil {
			return p, fmt.Errorf("policy.custom.outside_read: %w", err)
		}
		p.OutsideRead = a
	}
	if p.Rules["execute"] == Deny {
		p.ClaudeDisallowed = append(p.ClaudeDisallowed, "Bash")
	}
	if p.Rules["edit"] == Deny {
		p.ClaudeDisallowed = append(p.ClaudeDisallowed, "Write", "Edit", "MultiEdit", "NotebookEdit")
	}
	if p.Rules["fetch"] == Deny {
		p.ClaudeDisallowed = append(p.ClaudeDisallowed, "WebFetch", "WebSearch")
	}
	return p, nil
}

func parseAction(s string) (Action, error) {
	switch Action(s) {
	case Allow, Deny, Ask:
		return Action(s), nil
	}
	return "", fmt.Errorf("must be allow, deny or ask, not %q", s)
}

// Context is what a decision depends on besides the request.
type Context struct {
	Workspace string
	// Skills maps each delivered skill directory to its trust state.
	Skills map[string]domain.TrustState
}

var writeKinds = []string{"edit", "delete", "move"}

// Decide returns the action for a request and a reason for the audit log.
func (p Profile) Decide(req domain.ApprovalRequest, c Context) (Action, string) {
	kind := req.Kind
	if !slices.Contains(Kinds, kind) {
		kind = "other"
	}
	base := p.Rules[kind]

	paths := requestPaths(req.Input)
	for _, path := range paths {
		where := c.locate(path)
		switch {
		case slices.Contains(writeKinds, kind) && where == "skill":
			return Deny, "delivered skills are read-only"
		case slices.Contains(writeKinds, kind) && where == "outside":
			return Deny, "writes outside the workspace are not allowed"
		case (kind == "read" || kind == "search") && where == "skill":
			return Allow, "reading a delivered skill"
		case (kind == "read" || kind == "search") && where == "outside":
			return p.OutsideRead, "path outside the workspace"
		}
	}

	if kind == "execute" {
		if dir, trust, ok := c.scriptIn(req); ok {
			switch {
			case base == Deny:
				return Deny, "profile denies commands"
			case trust == domain.TrustTrusted:
				return base, "script of trusted skill " + filepath.Base(dir)
			case trust == domain.TrustReviewed:
				return Ask, "script of reviewed skill " + filepath.Base(dir)
			default:
				return Deny, "scripts of this skill are not allowed"
			}
		}
	}
	return base, "profile " + p.Name + " rule for " + kind
}

// locate classifies a path as "workspace", "skill" or "outside".
func (c Context) locate(p string) string {
	if !filepath.IsAbs(p) {
		if c.Workspace == "" {
			return "outside"
		}
		p = filepath.Join(c.Workspace, p)
	}
	p = filepath.Clean(p)
	for dir := range c.Skills {
		if within(dir, p) {
			return "skill"
		}
	}
	if c.Workspace != "" && within(c.Workspace, p) {
		return "workspace"
	}
	return "outside"
}

// Locate is locate for callers outside the package.
func (c Context) Locate(p string) string { return c.locate(p) }

func (c Context) scriptIn(req domain.ApprovalRequest) (string, domain.TrustState, bool) {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(req.Input, &in)
	text := in.Command + " " + req.Title
	for dir, t := range c.Skills {
		if strings.Contains(text, dir) {
			return dir, t, true
		}
	}
	return "", "", false
}

func within(root, p string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// requestPaths pulls file paths out of a tool's input.
func requestPaths(input json.RawMessage) []string {
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "filePath", "notebook_path", "abs_path"} {
		if s, ok := m[k].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Control is one line of the enforcement report.
type Control struct {
	Name   string `json:"name"`
	Status string `json:"status"` // enforced, runtime, not_enforced
	Detail string `json:"detail"`
}

// Enforcement reports, for a profile and integration, which controls rbridge
// enforces itself, which rely on the runtime asking permission, and which are
// not enforced at all.
// Sandbox describes the OS sandbox a session runs in, if any.
type Sandbox struct {
	Engine string // "bubblewrap", "docker" or "" for none
	Mode   string // "files" or "full"
}

func (s Sandbox) on() bool { return s.Engine != "" && (s.Mode == "files" || s.Mode == "full") }

// Enforcement reports, for one session, which controls rbridge enforces,
// which rely on the runtime asking, and which are not enforced.
func Enforcement(p Profile, integration domain.Integration, sb Sandbox) []Control {
	var out []Control
	add := func(name, status, detail string) { out = append(out, Control{name, status, detail}) }
	if integration == domain.IntegrationModelServer {
		add("tools", "enforced", "a model server generates text only; it cannot read or change files, run commands or fetch anything")
		add("network", "enforced", "prompts go only to the model server on this machine")
		return out
	}
	add("environment", "enforced", "runtime starts with an allowlisted environment")
	add("process lifetime", "enforced", "timeouts and process tree termination")
	add("skill scripts", "enforced", "scripts are not delivered unless trust allows them")
	if sb.on() {
		add("file writes", "enforced", sb.Engine+" sandbox: only the workspace, the session folder and the runtime's own folders are writable; permission requests still go through the profile")
	}
	switch integration {
	case domain.IntegrationACP:
		if !sb.on() {
			add("file writes", "runtime", "fs/write_text_file is checked by rbridge; the agent's own write tools rely on it asking permission")
		}
		if p.OffersTerminal() {
			add("client terminals", "enforced", "commands the agent runs through rbridge terminals are checked, time limited and confined to the workspace")
			add("commands", "runtime", "the agent's own shell relies on it asking permission")
		} else {
			add("commands", "runtime", "rbridge does not offer terminals; the agent's own shell relies on it asking permission")
		}
	case domain.IntegrationClaudeHeadless:
		if len(p.ClaudeDisallowed) > 0 {
			add("disallowed tools", "enforced", "removed with --disallowed-tools: "+strings.Join(p.ClaudeDisallowed, ", "))
		}
		if !sb.on() {
			add("file writes", "runtime", "Claude Code asks rbridge before edits")
		}
		add("commands", "runtime", "Claude Code asks rbridge before most shell commands; read-only commands may run without asking")
	}
	switch {
	case sb.on() && sb.Mode == "full":
		add("network", "enforced", sb.Engine+" sandbox with no network: the runtime cannot reach anything, its model API included")
	case p.BlockNetwork:
		add("network", "not_enforced", "no network sandbox for this session (sandbox mode full adds one); fetch tools are denied by rule, other traffic is not blocked")
	default:
		add("network", "not_enforced", "not restricted by this profile")
	}
	return out
}

// OffersTerminal says whether ACP agents are offered rbridge-run terminals:
// every profile that does not deny commands, since each command is checked.
func (p Profile) OffersTerminal() bool { return p.Rules["execute"] != Deny }

// Unenforced lists the controls a profile asks for that rbridge cannot enforce.
func Unenforced(p Profile, controls []Control) []string {
	var out []string
	for _, c := range controls {
		if c.Status == "not_enforced" && (c.Name != "network" || p.BlockNetwork) {
			out = append(out, c.Name)
		}
	}
	return out
}
