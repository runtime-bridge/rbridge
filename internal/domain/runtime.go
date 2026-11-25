package domain

import (
	"context"
	"encoding/json"
	"time"
)

// SessionOptions is everything a provider needs to start or resume a session.
type SessionOptions struct {
	// BridgeID is the bridge's own session id.
	BridgeID   string
	Workspace  string // absolute
	Executable string // absolute path chosen by detection
	// Env is the complete environment for the runtime process.
	Env []string
	// ResumeNativeID, when set, continues an earlier native session.
	ResumeNativeID string
	// SkillDirs are directories prepared for this session, in the layout the
	// provider asked for (see the manifest's session skill layout).
	SkillDirs  []string
	MCPServers []MCPServer
	Model      string
	// PermissionMode is the runtime-native permission mode derived from policy.
	PermissionMode string
	OutputSchema   json.RawMessage
	Approver       Approver
	// IdleTimeout ends a turn that produces no event for this long.
	IdleTimeout time.Duration
	// KillGrace is how long Close waits before killing the process tree.
	KillGrace time.Duration
	// ScratchDir is a private directory for this session's generated files.
	ScratchDir string
	// ClientFSWrite offers ACP agents fs/write_text_file.
	ClientFSWrite bool
	// FSGate checks file operations rbridge performs for the runtime.
	// kind is "read" or "edit". A nil gate allows everything inside the workspace.
	FSGate func(ctx context.Context, kind, path string) error
	// DisallowedTools are runtime tools to remove where the runtime supports it.
	DisallowedTools []string
}

// Turn is one prompt sent to a session.
type Turn struct {
	ID     string
	Prompt []ContentBlock
}

// TurnResult is what a provider reports at the end of a turn.
type TurnResult struct {
	StopReason StopReason
	Output     string
	Structured json.RawMessage
	Usage      *Usage
	// AuthMode is what the runtime revealed about its sign-in, if anything.
	AuthMode AuthMode
}

// Runtime starts sessions. Providers that can run tasks implement it in
// addition to Provider.
type Runtime interface {
	Provider
	Open(ctx context.Context, opts SessionOptions, sink EventSink) (ProviderSession, error)
}

// ProviderSession is one live native session.
type ProviderSession interface {
	NativeID() string
	// Capabilities returns what the runtime reported when the session started,
	// merged over the manifest values.
	Capabilities() CapabilitySet
	// Prompt runs one turn. Events are emitted on the session's sink until it
	// returns. A cancelled turn returns StopCancelled, not an error.
	Prompt(ctx context.Context, turn Turn) (TurnResult, error)
	// Cancel asks the runtime to stop the current turn.
	Cancel(ctx context.Context) error
	// Close ends the session and stops the runtime process tree.
	Close(ctx context.Context) error
}

// ApprovalRequest is a runtime asking permission for an action.
type ApprovalRequest struct {
	ID        string           `json:"id"`
	SessionID string           `json:"session_id"`
	Tool      string           `json:"tool,omitempty"`
	Title     string           `json:"title"`
	Kind      string           `json:"kind,omitempty"` // ACP tool kind: read, edit, delete, execute, ...
	Input     json.RawMessage  `json:"input,omitempty"`
	Options   []ApprovalOption `json:"options"`
}

// ApprovalOption is one answer the runtime accepts.
type ApprovalOption struct {
	ID   string `json:"id"`
	Kind string `json:"kind"` // allow_once, allow_always, reject_once, reject_always
	Name string `json:"name,omitempty"`
}

// ApprovalDecision answers an ApprovalRequest.
type ApprovalDecision struct {
	// OptionID is one of the request's options; empty means cancelled.
	OptionID string `json:"option_id,omitempty"`
	Allow    bool   `json:"allow"`
	By       string `json:"by"` // "user", "policy", "timeout", "session"
	Reason   string `json:"reason,omitempty"`
	// Remember allows the same tool and kind for the rest of this session
	// without asking again. It never outlives the session and never
	// overrides a policy denial.
	Remember bool `json:"remember,omitempty"`
}

// Approver decides approval requests. It must return when ctx is done.
type Approver interface {
	Decide(ctx context.Context, req ApprovalRequest) ApprovalDecision
}

// ApproverFunc adapts a function to Approver.
type ApproverFunc func(context.Context, ApprovalRequest) ApprovalDecision

func (f ApproverFunc) Decide(ctx context.Context, r ApprovalRequest) ApprovalDecision {
	return f(ctx, r)
}

// Choose returns the decision that picks the first option of one of the given kinds.
func (r ApprovalRequest) Choose(by string, allow bool, kinds ...string) ApprovalDecision {
	for _, k := range kinds {
		for _, o := range r.Options {
			if o.Kind == k {
				return ApprovalDecision{OptionID: o.ID, Allow: allow, By: by}
			}
		}
	}
	return ApprovalDecision{Allow: false, By: by, Reason: "no matching option"}
}

// AllowOnce and RejectOnce are the common decisions.
func (r ApprovalRequest) AllowOnce(by string) ApprovalDecision {
	return r.Choose(by, true, "allow_once", "allow_always")
}

func (r ApprovalRequest) RejectOnce(by string) ApprovalDecision {
	return r.Choose(by, false, "reject_once", "reject_always")
}

// Event payloads. Each event type has one data shape.

type SessionStartedData struct {
	NativeID string   `json:"native_id"`
	Model    string   `json:"model,omitempty"`
	Skills   []string `json:"skills,omitempty"` // skills the runtime reported, when it does
	AuthMode AuthMode `json:"auth_mode,omitempty"`
}

type TextData struct {
	Text string `json:"text"`
}

type ToolData struct {
	ID     string          `json:"id"`
	Title  string          `json:"title,omitempty"`
	Kind   string          `json:"kind,omitempty"`
	Status string          `json:"status,omitempty"` // pending, in_progress, completed, failed
	Input  json.RawMessage `json:"input,omitempty"`
}

type PlanData struct {
	Entries []PlanEntry `json:"entries"`
}

type PlanEntry struct {
	Content  string `json:"content"`
	Status   string `json:"status,omitempty"`
	Priority string `json:"priority,omitempty"`
}

type ApprovalResolvedData struct {
	ID       string           `json:"id"`
	Decision ApprovalDecision `json:"decision"`
}

type NoticeData struct {
	Message string `json:"message"`
	Level   string `json:"level,omitempty"` // info, warn
}

type TurnCompletedData struct {
	StopReason StopReason      `json:"stop_reason"`
	Output     string          `json:"output,omitempty"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
}

type TurnFailedData struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type SkillDeliveredData struct {
	SkillID string `json:"skill_id"`
	Digest  string `json:"digest"`
	Mode    string `json:"mode"`
	Scripts bool   `json:"scripts"`
}

// NewEvent builds an event with marshalled data. Sequence numbers and times
// are filled in by the session manager.
func NewEvent(t EventType, data any) Event {
	e := Event{Type: t}
	if data != nil {
		b, err := json.Marshal(data)
		if err == nil {
			e.Data = b
		}
	}
	return e
}

// WithRaw attaches the original provider message.
func (e Event) WithRaw(raw []byte) Event {
	if len(raw) > 0 && json.Valid(raw) {
		e.Raw = append(json.RawMessage(nil), raw...)
	}
	return e
}
