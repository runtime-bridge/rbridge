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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/suggest"
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

	terminals *terminals
	modeID    string
	modes     []domain.Mode
	options   []domain.ConfigOption
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

	path, args, env := opts.Executable, l.Args, opts.Env
	if opts.Launcher != nil {
		path, args, env = opts.Launcher.Main(path, args, env, opts.Workspace)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = opts.Workspace
	cmd.Env = env
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
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: s.clientCapabilities(),
		ClientInfo:         &sdk.Implementation{Name: name, Version: ver},
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
	var modes *sdk.SessionModeState
	switch {
	case s.opts.ResumeNativeID != "" && sc.Resume != nil:
		r, err := s.conn.ResumeSession(hctx, sdk.ResumeSessionRequest{Cwd: s.opts.Workspace, SessionId: sdk.SessionId(s.opts.ResumeNativeID), McpServers: mcp})
		if err != nil {
			return s.mapErr("session/resume", err)
		}
		s.nativeID = s.opts.ResumeNativeID
		configOptions, modes = r.ConfigOptions, r.Modes
	case s.opts.ResumeNativeID != "" && ir.AgentCapabilities.LoadSession:
		r, err := s.conn.LoadSession(hctx, sdk.LoadSessionRequest{Cwd: s.opts.Workspace, SessionId: sdk.SessionId(s.opts.ResumeNativeID), McpServers: mcp})
		if err != nil {
			return s.mapErr("session/load", err)
		}
		s.nativeID = s.opts.ResumeNativeID
		configOptions, modes = r.ConfigOptions, r.Modes
	case s.opts.ResumeNativeID != "":
		return domain.Errorf(domain.ErrCapabilityUnavailable, "runtime cannot resume or load sessions")
	default:
		r, err := s.conn.NewSession(hctx, sdk.NewSessionRequest{Cwd: s.opts.Workspace, McpServers: mcp})
		if err != nil {
			return s.mapErr("session/new", err)
		}
		s.nativeID = string(r.SessionId)
		configOptions, modes = r.ConfigOptions, r.Modes
	}
	s.setModes(modes)
	s.setOptions(configOptions)

	model := currentModel(configOptions)
	choices := modelChoices(configOptions)
	switch s.opts.Model {
	case domain.ModelLatest:
		// The runtime lists no release dates; its own default is the newest
		// model it recommends.
		s.opts.Model = ""
	case domain.ModelAuto:
		s.opts.Model = ""
		for _, c := range choices {
			if strings.EqualFold(c, "auto") || strings.HasPrefix(strings.ToLower(c), "auto") {
				s.opts.Model = c
				break
			}
		}
	}
	if s.opts.Model != "" && len(choices) > 0 && !slices.Contains(choices, s.opts.Model) {
		full, err := matchModel(s.opts.Model, choices)
		if err != nil {
			return err
		}
		s.opts.Model = full
	}
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
	s.terminals.closeAll()
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
	s.terminals.closeAll()
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

// matchModel finds the runtime's own name for a model given without its
// provider prefix (big-pickle for opencode/big-pickle), when only one fits.
func matchModel(want string, choices []string) (string, error) {
	short := func(c string) string { return c[strings.LastIndex(c, "/")+1:] }
	var hits []string
	for _, c := range choices {
		if strings.EqualFold(short(c), want) || strings.EqualFold(c, want) {
			hits = append(hits, c)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return "", domain.Errorf(domain.ErrInvalidInput, "model %q could be %s; give the full name", want, strings.Join(hits, " or "))
	}
	names := make([]string, 0, len(choices)*2)
	for _, c := range choices {
		names = append(names, c, short(c))
	}
	shown := choices
	more := ""
	if len(shown) > 10 {
		shown, more = shown[:10], fmt.Sprintf(" and %d more", len(choices)-10)
	}
	return "", domain.Errorf(domain.ErrInvalidInput, "this runtime has no model %q%s (it offers %s%s; or use latest or auto)",
		want, suggest.DidYouMean(want, names), strings.Join(shown, ", "), more)
}

// modelChoices lists the values of the runtime's model setting.
func modelChoices(opts []sdk.SessionConfigOption) []string {
	b, err := json.Marshal(opts)
	if err != nil {
		return nil
	}
	var raw []struct {
		ID      string `json:"id"`
		Options []struct {
			Value string `json:"value"`
		} `json:"options"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return nil
	}
	var out []string
	for _, o := range raw {
		if o.ID == "model" {
			for _, v := range o.Options {
				out = append(out, v.Value)
			}
		}
	}
	return out
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

func (s *Session) clientCapabilities() sdk.ClientCapabilities {
	c := sdk.ClientCapabilities{
		Fs:       sdk.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: s.opts.ClientFSWrite},
		Terminal: s.opts.ClientTerminal,
	}
	if s.opts.Elicitor != nil {
		c.Elicitation = &sdk.ElicitationCapabilities{Form: &sdk.ElicitationFormCapabilities{}, Url: &sdk.ElicitationUrlCapabilities{}}
	}
	return c
}

func (s *Session) setModes(m *sdk.SessionModeState) {
	if m == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modeID = string(m.CurrentModeId)
	s.modes = nil
	for _, x := range m.AvailableModes {
		mode := domain.Mode{ID: string(x.Id), Name: x.Name}
		if x.Description != nil {
			mode.Description = *x.Description
		}
		s.modes = append(s.modes, mode)
	}
}

// setOptions converts ACP config options through JSON, since the SDK keeps
// them as unions.
func (s *Session) setOptions(opts []sdk.SessionConfigOption) {
	b, err := json.Marshal(opts)
	if err != nil {
		return
	}
	var raw []struct {
		ID           string          `json:"id"`
		Name         string          `json:"name"`
		Description  string          `json:"description"`
		Type         string          `json:"type"`
		CurrentValue json.RawMessage `json:"currentValue"`
		Options      json.RawMessage `json:"options"`
	}
	if json.Unmarshal(b, &raw) != nil {
		return
	}
	var out []domain.ConfigOption
	for _, r := range raw {
		o := domain.ConfigOption{ID: r.ID, Name: r.Name, Description: r.Description, Type: r.Type}
		if o.Type == "" {
			o.Type = "select"
		}
		var str string
		var bl bool
		switch {
		case json.Unmarshal(r.CurrentValue, &str) == nil:
			o.Current = str
		case json.Unmarshal(r.CurrentValue, &bl) == nil:
			o.Current = fmt.Sprint(bl)
		}
		var flat []struct {
			Value   string            `json:"value"`
			Name    string            `json:"name"`
			Options []json.RawMessage `json:"options"`
		}
		if json.Unmarshal(r.Options, &flat) == nil {
			for _, f := range flat {
				if f.Value != "" {
					o.Options = append(o.Options, domain.ConfigOptionItem{Value: f.Value, Name: f.Name})
					continue
				}
				for _, g := range f.Options {
					var item domain.ConfigOptionItem
					if json.Unmarshal(g, &item) == nil && item.Value != "" {
						o.Options = append(o.Options, item)
					}
				}
			}
		}
		out = append(out, o)
	}
	s.mu.Lock()
	s.options = out
	s.mu.Unlock()
}

// Modes returns the current mode and the modes the agent offers.
func (s *Session) Modes() (string, []domain.Mode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.modeID, append([]domain.Mode(nil), s.modes...)
}

// SetMode switches the agent's session mode.
func (s *Session) SetMode(ctx context.Context, id string) error {
	_, modes := s.Modes()
	found := false
	for _, m := range modes {
		if m.ID == id {
			found = true
		}
	}
	if !found {
		return domain.Errorf(domain.ErrInvalidInput, "the runtime offers no mode %q", id)
	}
	if _, err := s.conn.SetSessionMode(ctx, sdk.SetSessionModeRequest{SessionId: sdk.SessionId(s.nativeID), ModeId: sdk.SessionModeId(id)}); err != nil {
		return s.mapErr("session/set_mode", err)
	}
	s.mu.Lock()
	s.modeID = id
	s.mu.Unlock()
	s.emit(domain.NewEvent(domain.EventModeUpdated, domain.ModeUpdatedData{Mode: id}))
	return nil
}

// ConfigOptions returns the agent's session settings.
func (s *Session) ConfigOptions() []domain.ConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.ConfigOption(nil), s.options...)
}

// SetConfigOption changes one session setting.
func (s *Session) SetConfigOption(ctx context.Context, id, value string) error {
	var opt *domain.ConfigOption
	for _, o := range s.ConfigOptions() {
		if o.ID == id {
			o := o
			opt = &o
		}
	}
	if opt == nil {
		return domain.Errorf(domain.ErrInvalidInput, "the runtime offers no option %q", id)
	}
	req := sdk.SetSessionConfigOptionRequest{}
	switch opt.Type {
	case "boolean":
		if value != "true" && value != "false" {
			return domain.Errorf(domain.ErrInvalidInput, "option %s takes true or false", id)
		}
		req.Boolean = &sdk.SetSessionConfigOptionBoolean{SessionId: sdk.SessionId(s.nativeID), ConfigId: sdk.SessionConfigId(id), Type: "boolean", Value: value == "true"}
	default:
		ok := len(opt.Options) == 0
		for _, item := range opt.Options {
			if item.Value == value {
				ok = true
			}
		}
		if !ok {
			return domain.Errorf(domain.ErrInvalidInput, "option %s has no value %q", id, value)
		}
		req.ValueId = &sdk.SetSessionConfigOptionValueId{SessionId: sdk.SessionId(s.nativeID), ConfigId: sdk.SessionConfigId(id), Value: sdk.SessionConfigValueId(value)}
	}
	r, err := s.conn.SetSessionConfigOption(ctx, req)
	if err != nil {
		return s.mapErr("session/set_config_option", err)
	}
	if len(r.ConfigOptions) > 0 {
		s.setOptions(r.ConfigOptions)
	} else {
		s.mu.Lock()
		for i := range s.options {
			if s.options[i].ID == id {
				s.options[i].Current = value
			}
		}
		s.mu.Unlock()
	}
	return nil
}

var _ domain.Extensions = (*Session)(nil)
