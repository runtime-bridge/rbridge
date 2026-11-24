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
	Workspace    string            `json:"workspace"`
	Prompt       []ContentBlock    `json:"prompt"`
	Skills       []string          `json:"skills,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Requirements []CapabilityName  `json:"requirements,omitempty"`
	Preferences  []CapabilityName  `json:"preferences,omitempty"`
	Profile      string            `json:"profile,omitempty"`
	MCPServers   []MCPServer       `json:"mcp_servers,omitempty"`
	OutputSchema json.RawMessage   `json:"output_schema,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// Validate checks the fields every provider relies on.
func (t Task) Validate() error {
	if t.Workspace == "" {
		return Errorf(ErrInvalidInput, "task workspace is required")
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
