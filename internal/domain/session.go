package domain

import (
	"encoding/json"
	"slices"
	"time"
)

// SessionState is the bridge-level lifecycle state of a session.
type SessionState string

const (
	StateCreated          SessionState = "created"
	StateStarting         SessionState = "starting"
	StateReady            SessionState = "ready"
	StateRunning          SessionState = "running"
	StateAwaitingApproval SessionState = "awaiting_approval"
	StateCancelling       SessionState = "cancelling"
	StateClosed           SessionState = "closed"
	StateFailed           SessionState = "failed"
	StateInterrupted      SessionState = "interrupted"
)

var sessionTransitions = map[SessionState][]SessionState{
	StateCreated:          {StateStarting, StateFailed, StateClosed},
	StateStarting:         {StateReady, StateFailed, StateClosed},
	StateReady:            {StateRunning, StateClosed, StateFailed, StateInterrupted},
	StateRunning:          {StateReady, StateAwaitingApproval, StateCancelling, StateFailed, StateInterrupted, StateClosed},
	StateAwaitingApproval: {StateRunning, StateCancelling, StateFailed, StateInterrupted, StateClosed},
	StateCancelling:       {StateReady, StateFailed, StateInterrupted, StateClosed},
	StateInterrupted:      {StateStarting, StateClosed},
}

// CanTransition reports whether a session may move from one state to another.
func CanTransition(from, to SessionState) bool {
	return slices.Contains(sessionTransitions[from], to)
}

// Terminal reports whether no further turns can run in this state.
func (s SessionState) Terminal() bool {
	return s == StateClosed || s == StateFailed
}

// StopReason says why a turn ended. Values follow ACP.
type StopReason string

const (
	StopEndTurn         StopReason = "end_turn"
	StopMaxTokens       StopReason = "max_tokens"
	StopMaxTurnRequests StopReason = "max_turn_requests"
	StopRefusal         StopReason = "refusal"
	StopCancelled       StopReason = "cancelled"
)

// SessionRecord is what the bridge stores about a session.
type SessionRecord struct {
	ID         string          `json:"id"`
	ProviderID string          `json:"provider_id"`
	NativeID   string          `json:"native_id,omitempty"`
	Workspace  string          `json:"workspace"`
	Profile    string          `json:"profile,omitempty"`
	Model      string          `json:"model,omitempty"`
	Skills     []SkillDelivery `json:"skills,omitempty"`
	State      SessionState    `json:"state"`
	Turns      int             `json:"turns"`
	LastStop   StopReason      `json:"last_stop,omitempty"`
	LastError  string          `json:"last_error,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Client     string          `json:"client,omitempty"`
	// OwnerPID is the rbridge process that holds the runtime session.
	OwnerPID int `json:"owner_pid,omitempty"`
	// Unenforced lists controls the profile wanted but rbridge could not enforce.
	Unenforced []string `json:"unenforced,omitempty"`
}

// Usage is token and cost information when a runtime reports it.
type Usage struct {
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
}

// SkillDelivery records how a skill was made available for a session.
type SkillDelivery struct {
	SkillID string `json:"skill_id"`
	Digest  string `json:"digest,omitempty"`
	Mode    string `json:"mode"`
}

// Result is the outcome of a task or a turn.
type Result struct {
	SessionID  string          `json:"session_id"`
	StopReason StopReason      `json:"stop_reason"`
	Output     string          `json:"output"`
	Structured json.RawMessage `json:"structured,omitempty"`
	Usage      *Usage          `json:"usage,omitempty"`
	Provider   string          `json:"provider"`
	Skills     []SkillDelivery `json:"skills,omitempty"`
	Errors     []*Error        `json:"errors,omitempty"`
}
