package domain

import (
	"context"
	"time"
)

// ProviderClass says what kind of runtime a provider wraps.
type ProviderClass string

const (
	ClassAgentRuntime ProviderClass = "agent-runtime"
	ClassModelServer  ProviderClass = "model-server"
	ClassCustom       ProviderClass = "custom"
)

// Integration is the protocol rbridge uses to talk to a runtime.
type Integration string

const (
	IntegrationACP            Integration = "acp"
	IntegrationClaudeHeadless Integration = "claude-headless"
)

// AuthMode is how a runtime is signed in, as far as rbridge can tell
// without reading credentials.
type AuthMode string

const (
	AuthSubscription AuthMode = "subscription"
	AuthAPIKey       AuthMode = "api-key"
	AuthLocal        AuthMode = "local"
	AuthUnknown      AuthMode = "unknown"
)

// Health is the result of detection.
type Health string

const (
	HealthAvailable Health = "available"
	HealthUnhealthy Health = "unhealthy"
	HealthMissing   Health = "missing"
	HealthDisabled  Health = "disabled"
)

// Installation is one copy of a runtime found on the machine.
type Installation struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RuntimeInfo is what detection found for one provider.
type RuntimeInfo struct {
	ProviderID    string         `json:"provider_id"`
	Health        Health         `json:"health"`
	Executable    string         `json:"executable,omitempty"`
	Version       string         `json:"version,omitempty"`
	Integration   Integration    `json:"integration"`
	AuthMode      AuthMode       `json:"auth_mode"`
	Installations []Installation `json:"installations,omitempty"`
	Pinned        bool           `json:"pinned,omitempty"`
	Notes         []string       `json:"notes,omitempty"`
	DetectedAt    time.Time      `json:"detected_at"`
}

// Provider is the contract every runtime adapter implements.
// Execution methods arrive with the provider adapters; detection and
// capabilities are enough for discovery.
type Provider interface {
	ID() string
	Detect(ctx context.Context) (RuntimeInfo, error)
	Capabilities(ctx context.Context) (CapabilitySet, error)
}
