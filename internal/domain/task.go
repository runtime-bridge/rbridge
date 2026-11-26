package domain

import (
	"encoding/json"
	"path/filepath"
	"time"
)

// ContentBlock is one piece of a prompt. Only text is required everywhere;
// other types depend on the runtime's prompt capabilities.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	URI      string `json:"uri,omitempty"`
	MimeType string `json:"mime_type,omitempty"`
	Data     string `json:"data,omitempty"`
}

// Text returns a single text content block.
func Text(s string) []ContentBlock { return []ContentBlock{{Type: "text", Text: s}} }

// MCPServer is an MCP server definition passed to a runtime with a task.
type MCPServer struct {
	Name    string            `json:"name"`
	Type    string            `json:"type,omitempty"` // "stdio" (default) or "http"
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Task is one request from an application.
type Task struct {
	Workspace    string           `json:"workspace"`
	Prompt       []ContentBlock   `json:"prompt"`
	Skills       []string         `json:"skills,omitempty"`
	Provider     string           `json:"provider,omitempty"`
	Requirements []CapabilityName `json:"requirements,omitempty"`
	Preferences  []CapabilityName `json:"preferences,omitempty"`
	Profile      string           `json:"profile,omitempty"`
	// Sandbox overrides the configured sandbox mode: off, files or full.
	Sandbox      string          `json:"sandbox,omitempty"`
	MCPServers   []MCPServer     `json:"mcp_servers,omitempty"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	Timeout      time.Duration   `json:"timeout,omitempty"`
	// SlotWait is how long the task queues for a free session slot: above
	// zero that long, below zero until one frees, zero execution.slot_wait.
	SlotWait time.Duration `json:"slot_wait,omitempty"`
	// Files are absolute paths of files the task brings. rbridge copies them
	// into a working folder of their own (the workspace, when none is given),
	// adds a text copy of each Word file, and tells the runtime about them.
	Files    []string          `json:"files,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Validate checks the fields every provider relies on.
func (t Task) Validate() error {
	if t.Workspace == "" {
		return Errorf(ErrInvalidInput, "task workspace is required")
	}
	for _, f := range t.Files {
		if !filepath.IsAbs(f) {
			return Errorf(ErrInvalidInput, "task files must be absolute paths: %q", f)
		}
	}
	if !filepath.IsAbs(t.Workspace) {
		return Errorf(ErrInvalidInput, "task workspace must be an absolute path: %q", t.Workspace)
	}
	if len(t.Prompt) == 0 {
		return Errorf(ErrInvalidInput, "task prompt is empty")
	}
	for _, b := range t.Prompt {
		if b.Type == "" {
			return Errorf(ErrInvalidInput, "prompt block without a type")
		}
	}
	if t.Timeout < 0 {
		return Errorf(ErrInvalidInput, "task timeout is negative")
	}
	return nil
}

// Model keywords a task may give instead of a model name. Each runtime turns
// them into one of its own models; see Manifest.ModelNames.
const (
	// ModelLatest is the runtime's newest flagship model.
	ModelLatest = "latest"
	// ModelAuto is the model the runtime itself recommends for the account
	// and the work, where it has such a choice, or else its default.
	ModelAuto = "auto"
)
