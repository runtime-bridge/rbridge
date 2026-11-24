// Package mcp exposes rbridge as a Model Context Protocol server over stdio,
// so another agent can list runtimes and skills and hand tasks to them.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/rbridge/rbridge/internal/buildinfo"
	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/service"
)

// Supported MCP protocol versions, newest first.
var versions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// Options configure the server.
type Options struct {
	// Profile applies to tasks that do not name one. Default "observe".
	Profile string
	// MaxTimeout caps run_task timeouts. Default 30 minutes.
	MaxTimeout time.Duration
}

type server struct {
	svc  *service.Service
	opts Options
	out  io.Writer
	mu   sync.Mutex
}

type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Serve answers MCP requests read from in until it ends.
func Serve(ctx context.Context, in io.Reader, out io.Writer, svc *service.Service, o Options) error {
	if o.Profile == "" {
		o.Profile = "observe"
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = 30 * time.Minute
	}
	s := &server{svc: svc, opts: o, out: out}
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	var wg sync.WaitGroup
	defer wg.Wait()
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			s.reply(nil, nil, &rpcErr{Code: -32700, Message: "parse error"})
			continue
		}
		if m.ID == nil {
			continue // notifications such as notifications/initialized
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := s.handle(ctx, m)
			s.reply(m.ID, res, err)
		}()
	}
	return sc.Err()
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *server) reply(id *json.RawMessage, result any, err *rpcErr) {
	msg := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		msg["error"] = err
	} else {
		msg["result"] = result
	}
	b, _ := json.Marshal(msg)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(b, '\n'))
}

func (s *server) handle(ctx context.Context, m message) (any, *rpcErr) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		v := versions[0]
		if slices.Contains(versions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "rbridge", "version": buildinfo.Get().Version},
			"instructions":    "Use rbridge_providers to see which agent runtimes are installed, rbridge_skills to see reviewed skills, and rbridge_run to hand a task to a runtime. Tasks run under the " + s.opts.Profile + " profile unless they name another; requests that would need a person to approve are denied.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, &rpcErr{Code: -32602, Message: "invalid params"}
		}
		text, err := s.call(ctx, p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}, "isError": false}, nil
	}
	return nil, &rpcErr{Code: -32601, Message: "method not found: " + m.Method}
}

func tools() []map[string]any {
	obj := func(props map[string]any, required ...string) map[string]any {
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	}
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	return []map[string]any{
		{
			"name":        "rbridge_providers",
			"description": "List the agent runtimes rbridge found on this machine, with status and version.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "rbridge_skills",
			"description": "List installed skills with their trust state. Only reviewed or trusted skills can be used.",
			"inputSchema": obj(map[string]any{}),
		},
		{
			"name":        "rbridge_run",
			"description": "Run a task on an installed agent runtime and return its answer.",
			"inputSchema": obj(map[string]any{
				"workspace":       str("absolute path of the project directory"),
				"prompt":          str("what the runtime should do"),
				"provider":        str("provider id; omit to use the first available"),
				"skills":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "skill ids to deliver"},
				"profile":         str("policy profile: observe, standard, controlled or custom"),
				"timeout_seconds": map[string]any{"type": "integer", "minimum": 1},
			}, "workspace", "prompt"),
		},
	}
}

func (s *server) call(ctx context.Context, name string, args json.RawMessage) (string, error) {
	switch name {
	case "rbridge_providers":
		list, err := s.svc.Providers(ctx, false)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, p := range list {
			fmt.Fprintf(&b, "%s: %s", p.ID, p.Runtime.Health)
			if p.Runtime.Version != "" {
				fmt.Fprintf(&b, " (version %s)", p.Runtime.Version)
			}
			b.WriteString("\n")
		}
		return b.String(), nil
	case "rbridge_skills":
		list, err := s.svc.Skills().List(ctx)
		if err != nil {
			return "", err
		}
		if len(list) == 0 {
			return "no skills installed", nil
		}
		var b strings.Builder
		for _, r := range list {
			fmt.Fprintf(&b, "%s (%s): %s\n", r.ID, r.Trust, r.Description)
		}
		return b.String(), nil
	case "rbridge_run":
		var a struct {
			Workspace      string   `json:"workspace"`
			Prompt         string   `json:"prompt"`
			Provider       string   `json:"provider"`
			Skills         []string `json:"skills"`
			Profile        string   `json:"profile"`
			TimeoutSeconds int      `json:"timeout_seconds"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", domain.Errorf(domain.ErrInvalidInput, "invalid arguments: %v", err)
		}
		if !filepath.IsAbs(a.Workspace) {
			return "", domain.Errorf(domain.ErrInvalidInput, "workspace must be an absolute path")
		}
		profile := a.Profile
		if profile == "" {
			profile = s.opts.Profile
		}
		timeout := time.Duration(a.TimeoutSeconds) * time.Second
		if timeout <= 0 || timeout > s.opts.MaxTimeout {
			timeout = s.opts.MaxTimeout
		}
		res, err := s.svc.Run(ctx, domain.Task{
			Workspace: a.Workspace, Prompt: domain.Text(a.Prompt), Provider: a.Provider,
			Skills: a.Skills, Profile: profile, Timeout: timeout,
		}, service.RunOptions{Client: "mcp"})
		if err != nil {
			return "", err
		}
		return res.Output, nil
	}
	return "", domain.Errorf(domain.ErrInvalidInput, "unknown tool %q", name)
}
