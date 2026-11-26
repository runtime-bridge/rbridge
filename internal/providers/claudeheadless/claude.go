// Package claudeheadless drives Claude Code in headless mode: `claude -p` with
// stream-json on stdin and stdout. Claude Code runs as installed and signed in
// by the user; this package never handles credentials.
package claudeheadless

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
)

// Session is one Claude Code process holding one conversation.
type Session struct {
	opts   domain.SessionOptions
	sink   domain.EventSink
	base   domain.CapabilitySet
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	waited chan error
	stderr *ring

	writeMu  sync.Mutex
	turnMu   sync.Mutex
	mu       sync.Mutex
	nativeID string
	started  bool
	authMode domain.AuthMode
	turn     *turnState
	controls map[string]chan json.RawMessage
	nextReq  int64

	activity atomic.Int64
	closed   atomic.Bool
	readDone chan struct{}
}

type turnState struct {
	result chan resultMsg
	text   strings.Builder
}

type resultMsg struct {
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StopReason       string          `json:"stop_reason"`
	TerminalReason   string          `json:"terminal_reason"`
	SessionID        string          `json:"session_id"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// Args builds the command line. base holds the manifest's launch arguments.
func Args(base []string, opts domain.SessionOptions, sessionID string, mcpConfig string) []string {
	args := append([]string(nil), base...)
	args = append(args, "--permission-prompts", "host", "--permission-prompt-tool", "stdio")
	if len(opts.DisallowedTools) > 0 {
		args = append(args, "--disallowed-tools")
		args = append(args, opts.DisallowedTools...)
	}
	mode := opts.PermissionMode
	if mode == "" {
		mode = "default"
	}
	args = append(args, "--permission-mode", mode)
	if opts.ResumeNativeID != "" {
		args = append(args, "--resume", opts.ResumeNativeID)
	} else {
		args = append(args, "--session-id", sessionID)
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if len(opts.OutputSchema) > 0 {
		args = append(args, "--json-schema", string(opts.OutputSchema))
	}
	if mcpConfig != "" {
		args = append(args, "--mcp-config", mcpConfig)
	}
	// --add-dir takes several values, so it goes last.
	if len(opts.SkillDirs) > 0 {
		args = append(args, "--add-dir")
		args = append(args, opts.SkillDirs...)
	}
	return args
}

// Open starts Claude Code and checks that it answers the initialize request.
func Open(ctx context.Context, base domain.CapabilitySet, launchArgs []string, opts domain.SessionOptions, sink domain.EventSink) (*Session, error) {
	if !filepath.IsAbs(opts.Workspace) {
		return nil, domain.Errorf(domain.ErrInvalidInput, "workspace must be absolute: %q", opts.Workspace)
	}
	if opts.Executable == "" {
		return nil, domain.Errorf(domain.ErrProviderUnavailable, "no executable")
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 5 * time.Second
	}
	s := &Session{
		opts: opts, sink: sink, base: base, stderr: &ring{max: 16 << 10},
		waited: make(chan error, 1), controls: map[string]chan json.RawMessage{},
		authMode: domain.AuthUnknown, readDone: make(chan struct{}),
	}
	s.touch()
	id := opts.ResumeNativeID
	if id == "" {
		id = newUUID()
	}
	s.nativeID = id

	mcpPath, err := writeMCPConfig(opts)
	if err != nil {
		return nil, err
	}
	path, args, env := opts.Executable, Args(launchArgs, opts, id, mcpPath), opts.Env
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
		return nil, &domain.Error{Code: domain.ErrProviderUnavailable, Message: "could not start Claude Code", Cause: err}
	}
	s.cmd, s.stdin = cmd, stdin
	go func() { s.waited <- cmd.Wait() }()
	go s.readLoop(stdout)

	ictx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := s.control(ictx, map[string]any{"subtype": "initialize"}); err != nil {
		s.kill()
		return nil, err
	}
	return s, nil
}

func (s *Session) NativeID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nativeID
}

func (s *Session) Capabilities() domain.CapabilitySet { return s.base.Merge(nil) }

// Prompt sends one user message and waits for its result.
func (s *Session) Prompt(ctx context.Context, turn domain.Turn) (domain.TurnResult, error) {
	s.turnMu.Lock()
	defer s.turnMu.Unlock()
	if s.closed.Load() {
		return domain.TurnResult{}, domain.Errorf(domain.ErrSessionNotFound, "session is closed")
	}
	content, err := userContent(turn.Prompt)
	if err != nil {
		return domain.TurnResult{}, err
	}
	ts := &turnState{result: make(chan resultMsg, 1)}
	s.mu.Lock()
	s.turn = ts
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn = nil
		s.mu.Unlock()
	}()
	s.touch()
	if err := s.write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": content}}); err != nil {
		return domain.TurnResult{}, s.exitErr()
	}

	idle := s.opts.IdleTimeout
	tick := time.NewTicker(watchInterval(idle))
	defer tick.Stop()
	var deadline <-chan time.Time
	var interrupted, idleFired bool
	for {
		select {
		case r := <-ts.result:
			if idleFired {
				return domain.TurnResult{}, domain.Errorf(domain.ErrExecutionTimeout, "Claude Code produced no output for %s", idle)
			}
			return s.finish(r, ts)
		case <-ctx.Done():
			if !interrupted {
				interrupted = true
				s.interrupt()
				deadline = time.After(s.opts.KillGrace)
			}
		case <-tick.C:
			if idle > 0 && !idleFired && time.Since(time.Unix(0, s.activity.Load())) > idle {
				idleFired = true
				s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: fmt.Sprintf("no output from Claude Code for %s; interrupting the turn", idle)}))
				s.interrupt()
				deadline = time.After(s.opts.KillGrace)
			}
		case <-deadline:
			// The runtime did not acknowledge the interrupt in time.
			s.kill()
			if idleFired {
				return domain.TurnResult{}, domain.Errorf(domain.ErrExecutionTimeout, "Claude Code produced no output for %s", idle)
			}
			return domain.TurnResult{StopReason: domain.StopCancelled, Output: s.textOf(ts)}, nil
		case <-s.readDone:
			select {
			case r := <-ts.result:
				return s.finish(r, ts)
			default:
			}
			if interrupted && !idleFired {
				return domain.TurnResult{StopReason: domain.StopCancelled, Output: s.textOf(ts)}, nil
			}
			return domain.TurnResult{}, s.exitErr()
		}
	}
}

func (s *Session) finish(r resultMsg, ts *turnState) (domain.TurnResult, error) {
	if r.SessionID != "" {
		s.mu.Lock()
		s.nativeID = r.SessionID
		s.mu.Unlock()
	}
	usage := &domain.Usage{InputTokens: r.Usage.InputTokens, OutputTokens: r.Usage.OutputTokens, CostUSD: r.TotalCostUSD}
	if r.Subtype == "error_during_execution" && r.TerminalReason == "aborted_streaming" {
		return domain.TurnResult{StopReason: domain.StopCancelled, Output: s.textOf(ts), Usage: usage}, nil
	}
	if r.IsError {
		code := domain.ErrExecutionFailed
		if authFailure(r.Result) {
			code = domain.ErrAuthenticationRequired
		}
		msg := strings.TrimSpace(r.Result)
		if msg == "" {
			msg = "Claude Code reported an error (" + r.Subtype + ")"
		}
		if strings.Contains(msg, "issue with the selected model") {
			code = domain.ErrInvalidInput
			msg += " Claude Code takes latest, auto, a family alias (opus, sonnet, haiku) or a full model name."
		}
		return domain.TurnResult{}, &domain.Error{Code: code, Message: msg}
	}
	stop := domain.StopEndTurn
	if r.StopReason == "max_tokens" {
		stop = domain.StopMaxTokens
	}
	out := r.Result
	if out == "" {
		out = s.textOf(ts)
	}
	s.mu.Lock()
	auth := s.authMode
	s.mu.Unlock()
	res := domain.TurnResult{StopReason: stop, Output: out, Usage: usage, AuthMode: auth}
	if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
		res.Structured = r.StructuredOutput
	} else if len(s.opts.OutputSchema) > 0 && json.Valid([]byte(strings.TrimSpace(out))) {
		res.Structured = json.RawMessage(strings.TrimSpace(out))
	}
	return res, nil
}

// Cancel interrupts the current turn.
func (s *Session) Cancel(context.Context) error {
	if s.closed.Load() {
		return nil
	}
	s.interrupt()
	return nil
}

func (s *Session) interrupt() {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.KillGrace)
		defer cancel()
		_, _ = s.control(ctx, map[string]any{"subtype": "interrupt"})
	}()
}

// Close ends the conversation and stops the process tree.
func (s *Session) Close(context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
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

func (s *Session) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.stdin.Write(append(b, '\n'))
	return err
}

// control sends a control request and waits for its response.
func (s *Session) control(ctx context.Context, req map[string]any) (json.RawMessage, error) {
	s.mu.Lock()
	s.nextReq++
	id := fmt.Sprintf("rb-%d", s.nextReq)
	ch := make(chan json.RawMessage, 1)
	s.controls[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.controls, id)
		s.mu.Unlock()
	}()
	if err := s.write(map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return nil, s.exitErr()
	}
	select {
	case r := <-ch:
		return r, nil
	case <-s.readDone:
		return nil, s.exitErr()
	case <-ctx.Done():
		return nil, &domain.Error{Code: domain.ErrProviderProtocol, Message: fmt.Sprintf("Claude Code did not answer %v", req["subtype"]), Details: s.details(), Cause: ctx.Err()}
	}
}

func (s *Session) emit(e domain.Event) {
	if s.sink != nil {
		s.sink.Emit(e)
	}
}

func (s *Session) touch() { s.activity.Store(time.Now().UnixNano()) }

func (s *Session) details() map[string]string {
	d := map[string]string{}
	if tail := s.stderr.String(); tail != "" {
		d["stderr_tail"] = tail
	}
	return d
}

func (s *Session) exitErr() error {
	select {
	case err := <-s.waited:
		s.waited <- err
	case <-time.After(2 * time.Second):
	}
	msg := "Claude Code exited"
	if s.cmd.ProcessState != nil {
		msg = fmt.Sprintf("Claude Code exited with code %d", s.cmd.ProcessState.ExitCode())
	}
	return &domain.Error{Code: domain.ErrExecutionFailed, Message: msg, Details: s.details()}
}

func (s *Session) readLoop(r io.Reader) {
	defer close(s.readDone)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		s.touch()
		line := append([]byte(nil), sc.Bytes()...)
		s.handle(line)
	}
}

func (s *Session) textOf(ts *turnState) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ts.text.String()
}

func (s *Session) current() *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

func (s *Session) handle(line []byte) {
	var head struct {
		Type      string          `json:"type"`
		Subtype   string          `json:"subtype"`
		RequestID string          `json:"request_id"`
		Request   json.RawMessage `json:"request"`
		Response  json.RawMessage `json:"response"`
	}
	if json.Unmarshal(line, &head) != nil {
		return
	}
	switch head.Type {
	case "control_response":
		var r struct {
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(head.Response, &r)
		s.mu.Lock()
		ch := s.controls[r.RequestID]
		s.mu.Unlock()
		if ch != nil {
			ch <- head.Response
		}
	case "control_request":
		go s.answerControl(head.RequestID, head.Request)
	case "system":
		s.handleSystem(head.Subtype, line)
	case "assistant":
		s.handleAssistant(line)
	case "user":
		s.handleToolResults(line)
	case "rate_limit_event":
		s.mu.Lock()
		if s.authMode == domain.AuthUnknown {
			s.authMode = domain.AuthSubscription
		}
		s.mu.Unlock()
	case "result":
		var r resultMsg
		_ = json.Unmarshal(line, &r)
		if ts := s.current(); ts != nil {
			select {
			case ts.result <- r:
			default:
			}
		}
	default:
		s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Message: "unmapped Claude Code message: " + head.Type, Level: "info"}).WithRaw(line))
	}
}

func (s *Session) handleSystem(subtype string, line []byte) {
	switch subtype {
	case "init":
		var in struct {
			SessionID    string   `json:"session_id"`
			Model        string   `json:"model"`
			Skills       []string `json:"skills"`
			APIKeySource string   `json:"apiKeySource"`
		}
		_ = json.Unmarshal(line, &in)
		s.mu.Lock()
		if in.SessionID != "" {
			s.nativeID = in.SessionID
		}
		if in.APIKeySource != "" && in.APIKeySource != "none" {
			s.authMode = domain.AuthAPIKey
		}
		first := !s.started
		s.started = true
		auth := s.authMode
		s.mu.Unlock()
		if first {
			s.emit(domain.NewEvent(domain.EventSessionStarted, domain.SessionStartedData{NativeID: in.SessionID, Model: in.Model, Skills: in.Skills, AuthMode: auth}).WithRaw(line))
		}
	case "permission_denied":
		var d struct {
			ToolName string `json:"tool_name"`
			Reason   string `json:"decision_reason"`
		}
		_ = json.Unmarshal(line, &d)
		s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: "Claude Code denied " + d.ToolName + ": " + d.Reason}).WithRaw(line))
	case "thinking_tokens":
	default:
		s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Message: "Claude Code system message: " + subtype, Level: "info"}).WithRaw(line))
	}
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

func (s *Session) handleAssistant(line []byte) {
	var m struct {
		Message struct {
			Content []contentBlock `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	for _, c := range m.Message.Content {
		switch c.Type {
		case "text":
			s.mu.Lock()
			if s.turn != nil {
				s.turn.text.WriteString(c.Text)
			}
			s.mu.Unlock()
			s.emit(domain.NewEvent(domain.EventMessageDelta, domain.TextData{Text: c.Text}).WithRaw(line))
		case "thinking":
			if c.Thinking != "" {
				s.emit(domain.NewEvent(domain.EventThoughtDelta, domain.TextData{Text: c.Thinking}).WithRaw(line))
			}
		case "tool_use":
			s.emit(domain.NewEvent(domain.EventToolStarted, domain.ToolData{ID: c.ID, Title: c.Name, Kind: toolKind(c.Name), Status: "in_progress", Input: c.Input}).WithRaw(line))
		}
	}
}

func (s *Session) handleToolResults(line []byte) {
	var m struct {
		Message struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &m) != nil {
		return
	}
	var blocks []contentBlock
	if json.Unmarshal(m.Message.Content, &blocks) != nil {
		return
	}
	for _, c := range blocks {
		if c.Type != "tool_result" {
			continue
		}
		status := "completed"
		if c.IsError {
			status = "failed"
		}
		s.emit(domain.NewEvent(domain.EventToolUpdated, domain.ToolData{ID: c.ToolUseID, Status: status}).WithRaw(line))
	}
}

// answerControl handles requests Claude Code sends to the host.
func (s *Session) answerControl(id string, req json.RawMessage) {
	var r struct {
		Subtype   string          `json:"subtype"`
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	_ = json.Unmarshal(req, &r)
	if r.Subtype != "can_use_tool" {
		_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{
			"subtype": "error", "request_id": id, "error": "rbridge does not handle " + r.Subtype,
		}})
		return
	}
	ar := domain.ApprovalRequest{
		ID:        firstNonEmpty(r.ToolUseID, id),
		SessionID: s.opts.BridgeID,
		Tool:      r.ToolName,
		Title:     approvalTitle(r.ToolName, r.Input),
		Kind:      toolKind(r.ToolName),
		Input:     r.Input,
		Options: []domain.ApprovalOption{
			{ID: "allow", Kind: "allow_once", Name: "Allow"},
			{ID: "deny", Kind: "reject_once", Name: "Deny"},
		},
	}
	var d domain.ApprovalDecision
	if s.opts.Approver == nil {
		d = ar.RejectOnce("policy")
		d.Reason = "no approver configured"
	} else {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			select {
			case <-s.readDone:
				cancel()
			case <-ctx.Done():
			}
		}()
		d = s.opts.Approver.Decide(ctx, ar)
		cancel()
	}
	s.touch()
	var resp map[string]any
	if d.Allow && d.OptionID == "allow" {
		input := r.Input
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		resp = map[string]any{"behavior": "allow", "updatedInput": input}
	} else {
		msg := d.Reason
		if msg == "" {
			msg = "denied by " + d.By
		}
		resp = map[string]any{"behavior": "deny", "message": msg}
	}
	_ = s.write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": id, "response": resp,
	}})
}

// authFailure recognises Claude Code's sign-in errors: not logged in, an
// expired or revoked login, or a rejected API key.
func authFailure(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"not logged in", "/login", "failed to authenticate", "oauth session expired", "oauth token", "invalid api key", "authentication_error", "invalid x-api-key"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

func approvalTitle(tool string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "path", "url", "pattern"} {
		if v, ok := in[k].(string); ok && v != "" {
			if len(v) > 200 {
				v = v[:200] + "..."
			}
			return tool + ": " + v
		}
	}
	return tool
}

// toolKind maps Claude Code tool names to ACP tool kinds so policy can treat
// both runtimes alike.
func toolKind(name string) string {
	switch name {
	case "Read", "NotebookRead":
		return "read"
	case "Write", "Edit", "MultiEdit", "NotebookEdit":
		return "edit"
	case "Bash", "BashOutput", "KillShell":
		return "execute"
	case "Glob", "Grep", "LS":
		return "search"
	case "WebFetch", "WebSearch":
		return "fetch"
	case "Skill":
		// Loading a skill reads its instructions; any script it then runs goes
		// through Bash and is judged as a command.
		return "read"
	}
	return "other"
}

func userContent(in []domain.ContentBlock) (any, error) {
	if len(in) == 1 && in[0].Type == "text" {
		return in[0].Text, nil
	}
	var out []map[string]any
	for _, b := range in {
		switch b.Type {
		case "text":
			out = append(out, map[string]any{"type": "text", "text": b.Text})
		case "image":
			out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": b.MimeType, "data": b.Data}})
		default:
			return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "prompt block type %q is not supported by Claude Code headless", b.Type)
		}
	}
	return out, nil
}

func writeMCPConfig(opts domain.SessionOptions) (string, error) {
	if len(opts.MCPServers) == 0 {
		return "", nil
	}
	if opts.ScratchDir == "" {
		return "", domain.Errorf(domain.ErrInternal, "MCP servers need a scratch directory")
	}
	servers := map[string]any{}
	for _, m := range opts.MCPServers {
		switch m.Type {
		case "", "stdio":
			if !filepath.IsAbs(m.Command) {
				return "", domain.Errorf(domain.ErrInvalidInput, "MCP server %q: command must be an absolute path", m.Name)
			}
			servers[m.Name] = map[string]any{"type": "stdio", "command": m.Command, "args": m.Args, "env": m.Env}
		case "http":
			servers[m.Name] = map[string]any{"type": "http", "url": m.URL, "headers": m.Headers}
		default:
			return "", domain.Errorf(domain.ErrInvalidInput, "MCP server %q: unsupported type %q", m.Name, m.Type)
		}
	}
	b, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return "", err
	}
	p := filepath.Join(opts.ScratchDir, "mcp.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		return "", err
	}
	return p, nil
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func watchInterval(idle time.Duration) time.Duration {
	if idle <= 0 {
		return time.Second
	}
	i := idle / 4
	if i < 10*time.Millisecond {
		return 10 * time.Millisecond
	}
	if i > time.Second {
		return time.Second
	}
	return i
}

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
