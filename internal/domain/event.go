package domain

import (
	"encoding/json"
	"time"
)

// EventType names a normalized event.
type EventType string

const (
	EventSessionStarted   EventType = "session.started"
	EventSkillDelivered   EventType = "skill.delivered"
	EventTurnStarted      EventType = "turn.started"
	EventMessageDelta     EventType = "message.delta"
	EventThoughtDelta     EventType = "thought.delta"
	EventPlanUpdated      EventType = "plan.updated"
	EventToolStarted      EventType = "tool.started"
	EventToolUpdated      EventType = "tool.updated"
	EventApprovalRequired EventType = "approval.required"
	EventApprovalResolved EventType = "approval.resolved"
	EventUsageUpdated     EventType = "usage.updated"
	EventNotice           EventType = "notice"
	EventTurnCompleted    EventType = "turn.completed"
	EventTurnFailed       EventType = "turn.failed"
	EventSessionClosed    EventType = "session.closed"
)

// Event is one normalized event in a session.
type Event struct {
	Seq       int64           `json:"seq"`
	Time      time.Time       `json:"time"`
	SessionID string          `json:"session_id"`
	TurnID    string          `json:"turn_id,omitempty"`
	Type      EventType       `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
	Provider  string          `json:"provider,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// EventSink receives events from a provider session. Implementations must be
// safe for use from several goroutines.
type EventSink interface {
	Emit(Event)
}

// EventSinkFunc adapts a function to EventSink.
type EventSinkFunc func(Event)

func (f EventSinkFunc) Emit(e Event) { f(e) }
