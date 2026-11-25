package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/daemon"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
)

// ConnectOptions locate a running daemon.
type ConnectOptions struct {
	// Home is the daemon's RBRIDGE_HOME, when it was started with one.
	Home string
	// Socket overrides the socket path.
	Socket string
	// Client names this application; sessions are visible only to the
	// client that opened them. Default "default".
	Client string
}

// Remote is a connection to an rbridge daemon. It implements Client.
type Remote struct {
	conn   net.Conn
	client string

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan rpcReply
	subs    map[string]*remoteSub
	done    chan struct{}
	err     error
}

var _ Client = (*Remote)(nil)

type rpcReply struct {
	Result json.RawMessage
	Err    error
}

type remoteSub struct {
	sink     domain.EventSink
	approver domain.Approver
	elicitor domain.Elicitor
}

// SocketPath returns the daemon socket for an RBRIDGE_HOME, or for the
// normal directories when home is empty.
func SocketPath(home string) (string, error) {
	getenv := os.Getenv
	if home != "" {
		getenv = func(k string) string {
			if k == "RBRIDGE_HOME" {
				return home
			}
			return os.Getenv(k)
		}
	}
	h, _ := os.UserHomeDir()
	paths, err := config.ResolvePaths(config.Env{GOOS: runtime.GOOS, HomeDir: h, UID: os.Getuid(), Getenv: getenv})
	if err != nil {
		return "", err
	}
	return daemon.SocketFor(daemon.FilesFor(paths)), nil
}

// Connect opens a connection to the daemon.
func Connect(ctx context.Context, o ConnectOptions) (*Remote, error) {
	sock := o.Socket
	if sock == "" {
		home := o.Home
		if home != "" {
			abs, err := filepath.Abs(home)
			if err != nil {
				return nil, err
			}
			home = abs
		}
		p, err := SocketPath(home)
		if err != nil {
			return nil, err
		}
		sock = p
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, &domain.Error{Code: domain.ErrProviderUnavailable, Message: "no rbridge daemon at " + sock + "; start one with rbridge daemon start", Cause: err}
	}
	client := o.Client
	if client == "" {
		client = "default"
	}
	r := &Remote{conn: conn, client: client, pending: map[int64]chan rpcReply{}, subs: map[string]*remoteSub{}, done: make(chan struct{})}
	go r.read()
	var init struct {
		ProtocolVersion int `json:"protocol_version"`
	}
	if err := r.call(ctx, "initialize", map[string]any{"client": client, "protocol_version": daemon.IPCProtocolVersion}, &init); err != nil {
		conn.Close()
		return nil, err
	}
	return r, nil
}

// Close ends the connection. Sessions left open keep running in the daemon
// until they are closed or idle long enough to be reaped.
func (r *Remote) Close() error { return r.conn.Close() }

func (r *Remote) read() {
	defer func() {
		r.mu.Lock()
		if r.err == nil {
			r.err = errors.New("connection to the daemon closed")
		}
		for id, ch := range r.pending {
			ch <- rpcReply{Err: &domain.Error{Code: domain.ErrProviderUnavailable, Message: r.err.Error()}}
			delete(r.pending, id)
		}
		r.mu.Unlock()
		close(r.done)
	}()
	sc := bufio.NewScanner(r.conn)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var m struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string           `json:"message"`
				Data    daemon.ErrorBody `json:"data"`
			} `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method != "" {
			r.notification(m.Method, m.Params)
			continue
		}
		if m.ID == nil {
			continue
		}
		r.mu.Lock()
		ch := r.pending[*m.ID]
		delete(r.pending, *m.ID)
		r.mu.Unlock()
		if ch == nil {
			continue
		}
		if m.Error != nil {
			code := m.Error.Data.Code
			if code == "" {
				code = domain.ErrInternal
			}
			ch <- rpcReply{Err: &domain.Error{Code: code, Message: m.Error.Data.Message, Details: m.Error.Data.Details}}
		} else {
			ch <- rpcReply{Result: m.Result}
		}
	}
}

func (r *Remote) notification(method string, params json.RawMessage) {
	if method != "session.event" {
		return
	}
	var p struct {
		Event domain.Event `json:"event"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	r.mu.Lock()
	sub := r.subs[p.Event.SessionID]
	r.mu.Unlock()
	if sub == nil {
		return
	}
	if sub.sink != nil {
		sub.sink.Emit(p.Event)
	}
	if p.Event.Type == domain.EventInputRequired && sub.elicitor != nil {
		var req domain.InputRequest
		if json.Unmarshal(p.Event.Data, &req) == nil {
			go func() {
				resp := sub.elicitor.Ask(context.Background(), req)
				_ = r.call(context.Background(), "inputs.answer", map[string]any{
					"session_id": p.Event.SessionID, "input_id": req.ID, "action": resp.Action, "content": resp.Content,
				}, nil)
			}()
		}
	}
	if p.Event.Type == domain.EventApprovalRequired {
		var req domain.ApprovalRequest
		if json.Unmarshal(p.Event.Data, &req) == nil {
			go r.answer(p.Event.SessionID, sub.approver, req)
		}
	}
}

func (r *Remote) answer(session string, approver domain.Approver, req domain.ApprovalRequest) {
	d := req.RejectOnce("client")
	if approver != nil {
		d = approver.Decide(context.Background(), req)
	}
	_ = r.call(context.Background(), "approvals.answer", map[string]any{
		"session_id": session, "approval_id": req.ID, "option_id": d.OptionID, "allow": d.Allow, "remember": d.Remember,
	}, nil)
}

func (r *Remote) call(ctx context.Context, method string, params any, out any) error {
	r.mu.Lock()
	if r.err != nil {
		err := r.err
		r.mu.Unlock()
		return &domain.Error{Code: domain.ErrProviderUnavailable, Message: err.Error()}
	}
	r.nextID++
	id := r.nextID
	ch := make(chan rpcReply, 1)
	r.pending[id] = ch
	r.mu.Unlock()
	b, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	r.writeMu.Lock()
	_, err = r.conn.Write(append(b, '\n'))
	r.writeMu.Unlock()
	if err != nil {
		return &domain.Error{Code: domain.ErrProviderUnavailable, Message: "write to daemon: " + err.Error()}
	}
	select {
	case rep := <-ch:
		if rep.Err != nil {
			return rep.Err
		}
		if out != nil && len(rep.Result) > 0 {
			return json.Unmarshal(rep.Result, out)
		}
		return nil
	case <-ctx.Done():
		r.mu.Lock()
		delete(r.pending, id)
		r.mu.Unlock()
		return ctx.Err()
	}
}

// Providers lists the daemon's providers.
func (r *Remote) Providers(ctx context.Context, refresh bool) ([]ProviderStatus, error) {
	var out []ProviderStatus
	return out, r.call(ctx, "providers.list", map[string]any{"refresh": refresh}, &out)
}

// Sessions lists this client's sessions.
func (r *Remote) Sessions(ctx context.Context) ([]SessionRecord, error) {
	var out []SessionRecord
	return out, r.call(ctx, "sessions.list", nil, &out)
}

// Status returns the daemon's status.
func (r *Remote) Status(ctx context.Context) (daemon.Status, error) {
	var s daemon.Status
	return s, r.call(ctx, "daemon.status", nil, &s)
}

func (r *Remote) attach(ctx context.Context, rec SessionRecord, ro service.RunOptions) (*Session, error) {
	r.mu.Lock()
	r.subs[rec.ID] = &remoteSub{sink: ro.Sink, approver: ro.Approver, elicitor: ro.Elicitor}
	r.mu.Unlock()
	if err := r.call(ctx, "sessions.subscribe", map[string]any{"id": rec.ID, "after": 0}, nil); err != nil {
		return nil, err
	}
	return &Session{&remoteSession{r: r, rec: rec}}, nil
}

// Open starts a session in the daemon.
func (r *Remote) Open(ctx context.Context, t Task, opts ...RunOption) (*Session, error) {
	ro := options(opts)
	if len(t.Prompt) == 0 {
		t.Prompt = Text("session")
	}
	var rec SessionRecord
	if err := r.call(ctx, "sessions.open", map[string]any{"task": t, "model": ro.Model, "mode": ro.Mode, "input": ro.Elicitor != nil}, &rec); err != nil {
		return nil, err
	}
	return r.attach(ctx, rec, ro)
}

// Resume reopens a stored session in the daemon.
func (r *Remote) Resume(ctx context.Context, id string, opts ...RunOption) (*Session, error) {
	ro := options(opts)
	var rec SessionRecord
	if err := r.call(ctx, "sessions.resume", map[string]any{"id": id, "model": ro.Model, "input": ro.Elicitor != nil}, &rec); err != nil {
		return nil, err
	}
	return r.attach(ctx, rec, ro)
}

// Run executes a task in a new daemon session and closes it.
func (r *Remote) Run(ctx context.Context, t Task, opts ...RunOption) (Result, error) {
	s, err := r.Open(ctx, t, opts...)
	if err != nil {
		return Result{}, err
	}
	defer s.Close(context.Background())
	return s.Prompt(ctx, t.Prompt)
}

type remoteSession struct {
	r   *Remote
	mu  sync.Mutex
	rec SessionRecord
}

func (s *remoteSession) record() SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec
}

func (s *remoteSession) capabilities() CapabilitySet { return nil }

func (s *remoteSession) prompt(ctx context.Context, p []ContentBlock) (Result, error) {
	var res Result
	err := s.r.call(ctx, "sessions.prompt", map[string]any{"id": s.rec.ID, "prompt": p}, &res)
	if errors.Is(err, context.Canceled) {
		_ = s.r.call(context.Background(), "sessions.cancel", map[string]any{"id": s.rec.ID}, nil)
	}
	var rec SessionRecord
	if s.r.call(context.Background(), "sessions.get", map[string]any{"id": s.rec.ID}, &rec) == nil {
		s.mu.Lock()
		s.rec = rec
		s.mu.Unlock()
	}
	return res, err
}

func (s *remoteSession) cancel(ctx context.Context) error {
	return s.r.call(ctx, "sessions.cancel", map[string]any{"id": s.rec.ID}, nil)
}

func (s *remoteSession) close(ctx context.Context) error {
	err := s.r.call(ctx, "sessions.close", map[string]any{"id": s.rec.ID}, nil)
	_ = s.r.call(ctx, "sessions.unsubscribe", map[string]any{"id": s.rec.ID}, nil)
	s.r.mu.Lock()
	delete(s.r.subs, s.rec.ID)
	s.r.mu.Unlock()
	if err != nil {
		return fmt.Errorf("close session: %w", err)
	}
	return nil
}

func (s *remoteSession) modes(ctx context.Context) (string, []Mode, error) {
	var st daemon.ModeState
	err := s.r.call(ctx, "sessions.modes", map[string]any{"id": s.rec.ID}, &st)
	return st.Current, st.Modes, err
}

func (s *remoteSession) setMode(ctx context.Context, id string) error {
	return s.r.call(ctx, "sessions.set_mode", map[string]any{"id": s.rec.ID, "mode": id}, nil)
}

func (s *remoteSession) options(ctx context.Context) ([]ConfigOption, error) {
	var out []ConfigOption
	return out, s.r.call(ctx, "sessions.options", map[string]any{"id": s.rec.ID}, &out)
}

func (s *remoteSession) setOption(ctx context.Context, id, value string) error {
	return s.r.call(ctx, "sessions.set_option", map[string]any{"id": s.rec.ID, "option_id": id, "value": value}, nil)
}
