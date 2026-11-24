// Package bridge lets Go programs use the agent runtimes installed on the
// machine through rbridge, without a daemon.
//
//	b, err := bridge.New(ctx, bridge.Options{})
//	if err != nil { ... }
//	res, err := b.Run(ctx, bridge.Task{
//		Workspace: "/home/me/project",
//		Prompt:    bridge.Text("Review this project for security issues."),
//		Skills:    []string{"security-audit"},
//	}, bridge.OnEvent(func(e bridge.Event) { log.Println(e.Type) }))
//
// The types are aliases of rbridge's internal types, so values move between
// this package and rbridge's JSON output unchanged.
package bridge

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/service"
	skillreg "github.com/rbridge/rbridge/internal/skills/registry"
)

type (
	Task             = domain.Task
	Result           = domain.Result
	Event            = domain.Event
	EventType        = domain.EventType
	ContentBlock     = domain.ContentBlock
	Capability       = domain.CapabilityName
	CapabilitySet    = domain.CapabilitySet
	ApprovalRequest  = domain.ApprovalRequest
	ApprovalDecision = domain.ApprovalDecision
	MCPServer        = domain.MCPServer
	Usage            = domain.Usage
	StopReason       = domain.StopReason
	SessionRecord    = domain.SessionRecord
	SkillRecord      = domain.SkillRecord
	TrustState       = domain.TrustState
	Error            = domain.Error
	ErrorCode        = domain.ErrorCode
	ProviderStatus   = service.ProviderStatus

	TextData           = domain.TextData
	ToolData           = domain.ToolData
	NoticeData         = domain.NoticeData
	SessionStartedData = domain.SessionStartedData
	SkillDeliveredData = domain.SkillDeliveredData
	TurnCompletedData  = domain.TurnCompletedData
	TurnFailedData     = domain.TurnFailedData
)

// Capabilities.
const (
	CapStreaming        = domain.CapStreaming
	CapSessionsResume   = domain.CapSessionsResume
	CapCancel           = domain.CapCancel
	CapSkills           = domain.CapSkills
	CapMCPStdio         = domain.CapMCPStdio
	CapFilesystem       = domain.CapFilesystem
	CapShell            = domain.CapShell
	CapStructuredOutput = domain.CapStructuredOutput
	CapApprovals        = domain.CapApprovals
	CapPromptImage      = domain.CapPromptImage
	CapOffline          = domain.CapOffline
)

// Event types.
const (
	EventSessionStarted   = domain.EventSessionStarted
	EventSkillDelivered   = domain.EventSkillDelivered
	EventTurnStarted      = domain.EventTurnStarted
	EventMessageDelta     = domain.EventMessageDelta
	EventThoughtDelta     = domain.EventThoughtDelta
	EventPlanUpdated      = domain.EventPlanUpdated
	EventToolStarted      = domain.EventToolStarted
	EventToolUpdated      = domain.EventToolUpdated
	EventApprovalRequired = domain.EventApprovalRequired
	EventApprovalResolved = domain.EventApprovalResolved
	EventUsageUpdated     = domain.EventUsageUpdated
	EventNotice           = domain.EventNotice
	EventTurnCompleted    = domain.EventTurnCompleted
	EventTurnFailed       = domain.EventTurnFailed
	EventSessionClosed    = domain.EventSessionClosed
)

// Stop reasons and trust states.
const (
	StopEndTurn   = domain.StopEndTurn
	StopCancelled = domain.StopCancelled

	TrustUnreviewed = domain.TrustUnreviewed
	TrustReviewed   = domain.TrustReviewed
	TrustTrusted    = domain.TrustTrusted
	TrustRestricted = domain.TrustRestricted
	TrustBlocked    = domain.TrustBlocked
)

// Error codes most callers handle.
const (
	ErrProviderNotFound       = domain.ErrProviderNotFound
	ErrProviderUnavailable    = domain.ErrProviderUnavailable
	ErrAuthenticationRequired = domain.ErrAuthenticationRequired
	ErrCapabilityUnavailable  = domain.ErrCapabilityUnavailable
	ErrSkillNotFound          = domain.ErrSkillNotFound
	ErrSkillBlocked           = domain.ErrSkillBlocked
	ErrSkillDigestMismatch    = domain.ErrSkillDigestMismatch
	ErrExecutionTimeout       = domain.ErrExecutionTimeout
	ErrExecutionFailed        = domain.ErrExecutionFailed
	ErrInvalidInput           = domain.ErrInvalidInput
)

// Text returns a prompt made of one text block.
func Text(s string) []ContentBlock { return domain.Text(s) }

// CodeOf returns the rbridge error code of err.
func CodeOf(err error) ErrorCode { return domain.CodeOf(err) }

// Options configure a Bridge. The zero value uses the user's normal rbridge
// configuration and directories.
type Options struct {
	// Home keeps all rbridge state under one directory, like RBRIDGE_HOME.
	Home string
	// ProjectDir is searched for .rbridge.yaml.
	ProjectDir string
	// Environ is the environment runtimes start from (default os.Environ).
	// rbridge still filters it through the policy allowlist.
	Environ []string
}

// Bridge is an rbridge instance running inside the calling process.
type Bridge struct {
	svc *service.Service
}

// New loads configuration and prepares a Bridge.
func New(_ context.Context, o Options) (*Bridge, error) {
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	vars := map[string]string{}
	for _, kv := range environ {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				vars[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	if o.Home != "" {
		h, err := filepath.Abs(o.Home)
		if err != nil {
			return nil, err
		}
		vars["RBRIDGE_HOME"] = h
	}
	home, _ := os.UserHomeDir()
	getenv := func(k string) string { return vars[k] }
	svc, err := service.FromEnvironment(service.Environment{
		Env:        config.Env{GOOS: runtime.GOOS, Getenv: getenv, HomeDir: home, UID: os.Getuid()},
		Environ:    func() []string { return environ },
		ProjectDir: o.ProjectDir,
	})
	if err != nil {
		return nil, err
	}
	return &Bridge{svc: svc}, nil
}

// Close releases resources. Sessions must be closed separately.
func (b *Bridge) Close() error { return nil }

// Providers lists the runtimes rbridge knows about and their status.
func (b *Bridge) Providers(ctx context.Context, refresh bool) ([]ProviderStatus, error) {
	return b.svc.Providers(ctx, refresh)
}

// RunOption adjusts one Run or Open call.
type RunOption func(*service.RunOptions)

// OnEvent receives every event of the session.
func OnEvent(f func(Event)) RunOption {
	return func(o *service.RunOptions) { o.Sink = domain.EventSinkFunc(f) }
}

// WithApprover answers the runtime's permission requests. Without it every
// request is denied.
func WithApprover(f func(context.Context, ApprovalRequest) ApprovalDecision) RunOption {
	return func(o *service.RunOptions) { o.Approver = domain.ApproverFunc(f) }
}

// AllowAll approves every request. Use it only for trusted tasks in throwaway workspaces.
func AllowAll() RunOption {
	return WithApprover(func(_ context.Context, r ApprovalRequest) ApprovalDecision { return r.AllowOnce("caller") })
}

// WithModel passes a model name to the runtime.
func WithModel(m string) RunOption { return func(o *service.RunOptions) { o.Model = m } }

func options(opts []RunOption) service.RunOptions {
	ro := service.RunOptions{Client: "go"}
	for _, o := range opts {
		o(&ro)
	}
	return ro
}

// Run executes a task in a new session and closes it.
func (b *Bridge) Run(ctx context.Context, t Task, opts ...RunOption) (Result, error) {
	return b.svc.Run(ctx, t, options(opts))
}

// Session is a multi-turn conversation with one runtime.
type Session struct{ s *service.Session }

// Open starts a session. Task.Prompt is not sent; call Prompt for each turn.
func (b *Bridge) Open(ctx context.Context, t Task, opts ...RunOption) (*Session, error) {
	if len(t.Prompt) == 0 {
		t.Prompt = Text("session")
	}
	s, err := b.svc.OpenSession(ctx, t, options(opts))
	if err != nil {
		return nil, err
	}
	return &Session{s}, nil
}

// Resume reopens a stored session.
func (b *Bridge) Resume(ctx context.Context, id string, opts ...RunOption) (*Session, error) {
	s, err := b.svc.ResumeSession(ctx, id, options(opts))
	if err != nil {
		return nil, err
	}
	return &Session{s}, nil
}

// ID is the bridge session id.
func (s *Session) ID() string { return s.s.Record().ID }

// Record returns the stored session record.
func (s *Session) Record() SessionRecord { return s.s.Record() }

// Capabilities returns what the runtime reported for this session.
func (s *Session) Capabilities() CapabilitySet { return s.s.Capabilities() }

// Prompt runs one turn.
func (s *Session) Prompt(ctx context.Context, prompt []ContentBlock) (Result, error) {
	return s.s.Prompt(ctx, prompt)
}

// Cancel stops the current turn.
func (s *Session) Cancel(ctx context.Context) error { return s.s.Cancel(ctx) }

// Close ends the session and stops the runtime.
func (s *Session) Close(ctx context.Context) error { return s.s.Close(ctx) }

// Sessions lists stored sessions, newest first.
func (b *Bridge) Sessions(ctx context.Context) ([]SessionRecord, error) { return b.svc.Sessions(ctx) }

// AddSkill installs a skill from a local directory. With link set the
// directory is used in place.
func (b *Bridge) AddSkill(ctx context.Context, dir string, link bool) (SkillRecord, error) {
	res, err := b.svc.Skills().Add(ctx, skillreg.AddRequest{Path: dir, Link: link})
	return res.Record, err
}

// AddSkillFromGit installs a skill from a Git repository at a pinned commit.
func (b *Bridge) AddSkillFromGit(ctx context.Context, url, commit, subdir string) (SkillRecord, error) {
	res, err := b.svc.Skills().Add(ctx, skillreg.AddRequest{URL: url, Rev: commit, Subdir: subdir})
	return res.Record, err
}

// Skills lists installed skills.
func (b *Bridge) Skills(ctx context.Context) ([]SkillRecord, error) { return b.svc.Skills().List(ctx) }

// SetSkillTrust changes a skill's trust state.
func (b *Bridge) SetSkillTrust(ctx context.Context, id string, t TrustState) (SkillRecord, error) {
	return b.svc.Skills().SetTrust(ctx, id, t)
}

// RemoveSkill deletes a skill from the library.
func (b *Bridge) RemoveSkill(ctx context.Context, id string) error {
	return b.svc.Skills().Remove(ctx, id)
}
