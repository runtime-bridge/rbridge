// Package acp runs agent runtimes over the Agent Client Protocol. rbridge is
// the client; the runtime is started as a subprocess speaking ACP on stdio.
// ACP types stay inside this package.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/process"
)

// Launch is how to start the agent process.
type Launch struct {
	Args []string
	// ClientName is sent in clientInfo.
	ClientName    string
	ClientVersion string
}

// Session is one ACP agent subprocess with one ACP session.
type Session struct {
	opts   domain.SessionOptions
	sink   domain.EventSink
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waited chan error
	conn   *sdk.ClientSideConnection
	stderr *ring

	nativeID string
	caps     domain.CapabilitySet
	canClose bool

	turnMu   sync.Mutex // one prompt at a time
	mu       sync.Mutex
	text     strings.Builder
	activity atomic.Int64 // unix nanos of the last agent message
	closed   atomic.Bool
}

// Open starts the agent, runs the handshake and creates or resumes a session.
func Open(ctx context.Context, base domain.CapabilitySet, l Launch, opts domain.SessionOptions, sink domain.EventSink) (*Session, error) {
	if !filepath.IsAbs(opts.Workspace) {
		return nil, domain.Errorf(domain.ErrInvalidInput, "workspace must be absolute: %q", opts.Workspace)
	}
	if opts.Executable == "" {
		return nil, domain.Errorf(domain.ErrProviderUnavailable, "no executable")
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 5 * time.Second
	}
	s := &Session{opts: opts, sink: sink, stderr: &ring{max: 16 << 10}, waited: make(chan error, 1)}
	s.touch()

	cmd := exec.Command(opts.Executable, l.Args...)
	cmd.Dir = opts.Workspace
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stderr = s.stderr
	process.Prepare(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, &domain.Error{Code: domain.ErrProviderUnavailable, Message: "could not start the runtime", Cause: err}
	}
	s.cmd, s.stdin = cmd, stdin
	go func() { s.waited <- cmd.Wait() }()
	s.conn = sdk.NewClientSideConnection(&client{s: s}, stdin, &activityReader{r: stdout, s: s})
	s.conn.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := s.handshake(ctx, l); err != nil {
		s.kill()
		return nil, err
	}
	s.caps = base.Merge(s.caps)
	return s, nil
}

func (s *Session) handshake(ctx context.Context, l Launch) error {
	hctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	name := l.ClientName
	if name == "" {
		name = "rbridge"
	}
	ver := l.ClientVersion
	ir, err := s.conn.Initialize(hctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{
			Fs: sdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
		},
		ClientInfo: &sdk.Implementation{Name: name, Version: ver},
	})
	if err != nil {
		return s.mapErr("initialize", err)
	}
	if ir.ProtocolVersion != sdk.ProtocolVersionNumber {
		return domain.Errorf(domain.ErrProviderProtocol, "runtime speaks ACP version %d, rbridge speaks %d", ir.ProtocolVersion, sdk.ProtocolVersionNumber)
	}
	s.caps = handshakeCapabilities(ir.AgentCapabilities)
	sc := ir.AgentCapabilities.SessionCapabilities
	s.canClose = sc.Close != nil

	mcp, err := mcpServers(s.opts.MCPServers, ir.AgentCapabilities.McpCapabilities)
	if err != nil {
		return err
	}
	var configOptions []sdk.SessionConfigOption
	switch {
	case s.opts.ResumeNativeID != "" && sc.Resume != nil:
		r, err := s.conn.ResumeSession(hctx, sdk.ResumeSessionRequest{Cwd: s.opts.Workspace, SessionId: sdk.SessionId(s.opts.ResumeNativeID), McpServers: mcp})
		if err != nil {
			return s.mapErr("session/resume", err)
		}
		s.nativeID = s.opts.ResumeNativeID
		configOptions = r.ConfigOptions
	case s.opts.ResumeNativeID != "" && ir.AgentCapabilities.LoadSession:
		r, err := s.conn.LoadSession(hctx, sdk.LoadSessionRequest{Cwd: s.opts.Workspace, SessionId: sdk.SessionId(s.opts.ResumeNativeID), McpServers: mcp})
		if err != nil {
			return s.mapErr("session/load", err)
		}
		s.nativeID = s.opts.ResumeNativeID
		configOptions = r.ConfigOptions
	case s.opts.ResumeNativeID != "":
		return domain.Errorf(domain.ErrCapabilityUnavailable, "runtime cannot resume or load sessions")
	default:
		r, err := s.conn.NewSession(hctx, sdk.NewSessionRequest{Cwd: s.opts.Workspace, McpServers: mcp})
		if err != nil {
			return s.mapErr("session/new", err)
		}
		s.nativeID = string(r.SessionId)
		configOptions = r.ConfigOptions
	}

	model := currentModel(configOptions)
	if s.opts.Model != "" {
		_, err := s.conn.SetSessionConfigOption(hctx, sdk.SetSessionConfigOptionRequest{ValueId: &sdk.SetSessionConfigOptionValueId{
			SessionId: sdk.SessionId(s.nativeID), ConfigId: "model", Value: sdk.SessionConfigValueId(s.opts.Model),
		}})
		if err != nil {
			return &domain.Error{Code: domain.ErrInvalidInput, Message: fmt.Sprintf("runtime rejected model %q", s.opts.Model), Cause: err}
		}
		model = s.opts.Model
	}
	s.emit(domain.NewEvent(domain.EventSessionStarted, domain.SessionStartedData{NativeID: s.nativeID, Model: model}))
	return nil
}

// NativeID returns the ACP session id.
func (s *Session) NativeID() string { return s.nativeID }

// Capabilities returns the merged capabilities.
func (s *Session) Capabilities() domain.CapabilitySet { return s.caps }

// Prompt runs one turn.
func (s *Session) Prompt(ctx context.Context, turn domain.Turn) (domain.TurnResult, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.closed.Load() {
		return domain.TurnResult{}, domain.Errorf(domain.ErrSessionNotFound, "session is closed")
	}
	blocks, err := contentBlocks(turn.Prompt)
	if err != nil {
		return domain.TurnResult{}, err
	}
	s.mu.Lock()
	s.text.Reset()
	s.mu.Unlock()
	s.touch()

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type res struct {
		r   sdk.PromptResponse
		err error
	}
	done := make(chan res, 1)
	go func() {
		r, err := s.conn.Prompt(pctx, sdk.PromptRequest{SessionId: sdk.SessionId(s.nativeID), Prompt: blocks})
		done <- res{r, err}
	}()

	idle := s.opts.IdleTimeout
	tick := time.NewTicker(watchInterval(idle))
	defer tick.Stop()
	var idleFired, cancelSent bool
	for {
		select {
		case r := <-done:
			if idleFired {
				return domain.TurnResult{}, domain.Errorf(domain.ErrExecutionTimeout, "runtime produced no output for %s", idle)
			}
			if r.err != nil {
				if ctx.Err() != nil {
					return domain.TurnResult{StopReason: domain.StopCancelled, Output: s.output()}, nil
				}
				return domain.TurnResult{}, s.mapErr("session/prompt", r.err)
			}
			out := domain.TurnResult{StopReason: domain.StopReason(r.r.StopReason), Output: s.output()}
			if u := r.r.Usage; u != nil {
				out.Usage = &domain.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
			}
			return out, nil
		case <-ctx.Done():
			if !cancelSent {
				cancelSent = true
				_ = s.Cancel(context.Background())
				// Give the runtime the grace period to answer the prompt with "cancelled".
				go func() {
					time.Sleep(s.opts.KillGrace)
					cancel()
				}()
			}
		case <-tick.C:
			if idle > 0 && !idleFired && time.Since(time.Unix(0, s.activity.Load())) > idle {
				idleFired = true
				s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: fmt.Sprintf("no output from the runtime for %s; cancelling the turn", idle)}))
				_ = s.Cancel(context.Background())
				go func() {
					time.Sleep(s.opts.KillGrace)
					cancel()
				}()
			}
		case err := <-s.waited:
			s.waited <- err
			return domain.TurnResult{}, s.exitErr(err)
		}
	}
}

// Cancel sends session/cancel.
func (s *Session) Cancel(ctx context.Context) error {
	if s.closed.Load() {
		return nil
	}
	return s.conn.Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(s.nativeID)})
}

// Close ends the session and stops the process tree.
func (s *Session) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	if s.canClose {
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, _ = s.conn.CloseSession(cctx, sdk.CloseSessionRequest{SessionId: sdk.SessionId(s.nativeID)})
		cancel()
	}
	_ = s.stdin.Close()
	select {
	case <-s.waited:
		process.KillGroup(s.cmd)
		return nil
	case <-time.After(s.opts.KillGrace):
	}
	_ = process.StopTree(s.cmd, s.waited, s.opts.KillGrace)
	return nil
}

func (s *Session) kill() {
	s.closed.Store(true)
	_ = s.stdin.Close()
	_ = process.StopTree(s.cmd, s.waited, time.Second)
}

func (s *Session) emit(e domain.Event) {
	if s.sink != nil {
		e.Provider = ""
		s.sink.Emit(e)
	}
}

func (s *Session) touch() { s.activity.Store(time.Now().UnixNano()) }

func (s *Session) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// mapErr turns protocol errors into bridge errors, keeping stderr for diagnosis.
func (s *Session) mapErr(step string, err error) error {
	var re *sdk.RequestError
	details := map[string]string{"step": step}
	if tail := s.stderr.String(); tail != "" {
		details["stderr_tail"] = tail
	}
	if errors.As(err, &re) {
		code := domain.ErrExecutionFailed
		switch re.Code {
		case -32000:
			code = domain.ErrAuthenticationRequired
		case -32601, -32602, -32700, -32600:
			code = domain.ErrProviderProtocol
		}
		return &domain.Error{Code: code, Message: fmt.Sprintf("%s: %s", step, re.Message), Details: details, Cause: err}
	}
	select {
	case werr := <-s.waited:
		s.waited <- werr
		return s.exitErr(werr)
	default:
	}
	return &domain.Error{Code: domain.ErrProviderProtocol, Message: fmt.Sprintf("%s: %v", step, err), Details: details, Cause: err}
}

func (s *Session) exitErr(err error) error {
	msg := "the runtime exited"
	if s.cmd.ProcessState != nil {
		msg = fmt.Sprintf("the runtime exited with code %d", s.cmd.ProcessState.ExitCode())
	}
	d := map[string]string{}
	if tail := s.stderr.String(); tail != "" {
		d["stderr_tail"] = tail
	}
	return &domain.Error{Code: domain.ErrExecutionFailed, Message: msg, Details: d, Cause: err}
}

func watchInterval(idle time.Duration) time.Duration {
	if idle <= 0 {
		return time.Second
	}
	if i := idle / 4; i < time.Second {
		if i < 10*time.Millisecond {
			return 10 * time.Millisecond
		}
		return i
	}
	return time.Second
}

func handshakeCapabilities(a sdk.AgentCapabilities) domain.CapabilitySet {
	b := func(v bool) domain.Support {
		if v {
			return domain.SupportYes
		}
		return domain.SupportNo
	}
	c := func(sup domain.Support, mode string) domain.Capability {
		return domain.Capability{Supported: sup, Mode: mode, Source: domain.SourceHandshake}
	}
	sc := a.SessionCapabilities
	return domain.CapabilitySet{
		domain.CapStreaming:          c(domain.SupportYes, "acp session/update"),
		domain.CapSessionsPersistent: c(domain.SupportYes, ""),
		domain.CapSessionsLoad:       c(b(a.LoadSession), "session/load"),
		domain.CapSessionsResume:     c(b(sc.Resume != nil), "session/resume"),
		domain.CapCancel:             c(domain.SupportYes, "session/cancel"),
		domain.CapMCPStdio:           c(domain.SupportYes, ""),
		domain.CapMCPHTTP:            c(b(a.McpCapabilities.Http), ""),
		domain.CapPromptImage:        c(b(a.PromptCapabilities.Image), ""),
		domain.CapPromptAudio:        c(b(a.PromptCapabilities.Audio), ""),
	}
}

func currentModel(opts []sdk.SessionConfigOption) string {
	b, err := json.Marshal(opts)
	if err != nil {
		return ""
	}
	var raw []struct {
		ID           string `json:"id"`
		CurrentValue string `json:"currentValue"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return ""
	}
	for _, o := range raw {
		if o.ID == "model" {
			return o.CurrentValue
		}
	}
	return ""
}

func contentBlocks(in []domain.ContentBlock) ([]sdk.ContentBlock, error) {
	out := make([]sdk.ContentBlock, 0, len(in))
	for _, b := range in {
		switch b.Type {
		case "text":
			out = append(out, sdk.TextBlock(b.Text))
		case "image":
			out = append(out, sdk.ImageBlock(b.Data, b.MimeType))
		case "resource_link":
			out = append(out, sdk.ResourceLinkBlock(b.Text, b.URI))
		default:
			return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "prompt block type %q is not supported over ACP", b.Type)
		}
	}
	return out, nil
}

func mcpServers(in []domain.MCPServer, caps sdk.McpCapabilities) ([]sdk.McpServer, error) {
	out := []sdk.McpServer{}
	for _, m := range in {
		switch m.Type {
		case "", "stdio":
			if !filepath.IsAbs(m.Command) {
				return nil, domain.Errorf(domain.ErrInvalidInput, "MCP server %q: command must be an absolute path", m.Name)
			}
			env := []sdk.EnvVariable{}
			for k, v := range m.Env {
				env = append(env, sdk.EnvVariable{Name: k, Value: v})
			}
			args := m.Args
			if args == nil {
				args = []string{}
			}
			out = append(out, sdk.McpServer{Stdio: &sdk.McpServerStdio{Name: m.Name, Command: m.Command, Args: args, Env: env}})
		case "http":
			if !caps.Http {
				return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "MCP server %q: runtime does not accept HTTP MCP servers", m.Name)
			}
			headers := []sdk.HttpHeader{}
			for k, v := range m.Headers {
				headers = append(headers, sdk.HttpHeader{Name: k, Value: v})
			}
			out = append(out, sdk.McpServer{Http: &sdk.McpServerHttpInline{Type: "http", Name: m.Name, Url: m.URL, Headers: headers}})
		default:
			return nil, domain.Errorf(domain.ErrInvalidInput, "MCP server %q: unsupported type %q", m.Name, m.Type)
		}
	}
	return out, nil
}

// activityReader records when the agent last sent anything.
type activityReader struct {
	r io.Reader
	s *Session
}

func (a *activityReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.s.touch()
	}
	return n, err
}

// ring keeps the last max bytes written to it.
type ring struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		r.buf = r.buf[len(r.buf)-r.max:]
	}
	return len(p), nil
}

func (r *ring) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.TrimSpace(string(r.buf))
}
