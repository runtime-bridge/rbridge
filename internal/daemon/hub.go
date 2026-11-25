// Package daemon serves rbridge to several applications at once over local
// HTTP and IPC. Both transports call the same Hub.
package daemon

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
)

// Hub owns the live sessions of the daemon.
type Hub struct {
	svc         *service.Service
	bufSize     int
	idleTimeout time.Duration
	// PerClient limits open sessions per client; 0 means no limit.
	PerClient int
	now       func() time.Time

	mu       sync.Mutex
	sessions map[string]*live
	closed   bool
}

// NewHub builds a hub over a service.
func NewHub(svc *service.Service, bufSize int, idle time.Duration) *Hub {
	return &Hub{svc: svc, bufSize: bufSize, idleTimeout: idle, now: time.Now, sessions: map[string]*live{}}
}

// clientTag stores the owning client in session records.
func clientTag(owner string) string { return "daemon:" + owner }

type live struct {
	hub   *Hub
	owner string
	sess  *service.Session

	mu        sync.Mutex
	id        string
	events    []domain.Event
	subs      map[int]chan domain.Event
	nextSub   int
	pending   map[string]chan domain.ApprovalDecision
	requests  map[string]domain.ApprovalRequest
	inputs    map[string]domain.InputRequest
	inputWait map[string]chan domain.InputResponse
	busy      bool
	cancel    context.CancelFunc
	lastUsed  time.Time
	closed    bool
	closedSig chan struct{}
}

func (l *live) Emit(e domain.Event) {
	l.mu.Lock()
	if l.id == "" {
		l.id = e.SessionID
	}
	l.events = append(l.events, e)
	if over := len(l.events) - l.hub.bufSize; over > 0 {
		l.events = append([]domain.Event(nil), l.events[over:]...)
	}
	for id, ch := range l.subs {
		select {
		case ch <- e:
		default:
			// A subscriber that cannot keep up is dropped; it can reconnect with its last sequence number.
			close(ch)
			delete(l.subs, id)
		}
	}
	l.mu.Unlock()
}

// Decide parks an approval request until the owning client answers it.
func (l *live) Decide(ctx context.Context, req domain.ApprovalRequest) domain.ApprovalDecision {
	ch := make(chan domain.ApprovalDecision, 1)
	l.mu.Lock()
	l.pending[req.ID] = ch
	l.requests[req.ID] = req
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, req.ID)
		delete(l.requests, req.ID)
		l.mu.Unlock()
	}()
	select {
	case d := <-ch:
		return d
	case <-ctx.Done():
		d := req.RejectOnce("timeout")
		d.Reason = "no answer from the client"
		return d
	case <-l.closedSig:
		d := req.RejectOnce("policy")
		d.Reason = "session closed"
		return d
	}
}

// Ask parks a question from the runtime until the owning client answers it.
func (l *live) Ask(ctx context.Context, req domain.InputRequest) domain.InputResponse {
	ch := make(chan domain.InputResponse, 1)
	l.mu.Lock()
	l.inputs[req.ID] = req
	l.inputWait[req.ID] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.inputs, req.ID)
		delete(l.inputWait, req.ID)
		l.mu.Unlock()
	}()
	select {
	case r := <-ch:
		return r
	case <-ctx.Done():
		return domain.InputResponse{Action: "cancel"}
	case <-l.closedSig:
		return domain.InputResponse{Action: "cancel"}
	}
}

func (l *live) touch() {
	l.mu.Lock()
	l.lastUsed = l.hub.now()
	l.mu.Unlock()
}

// OpenRequest starts a session for a client.
type OpenRequest struct {
	Task  domain.Task `json:"task"`
	Model string      `json:"model,omitempty"`
	// Mode is a runtime session mode to switch to once the session opens.
	Mode string `json:"mode,omitempty"`
	// Input says the client answers input.required events. Without it the
	// runtime's questions are declined at once.
	Input bool `json:"input,omitempty"`
}

func (h *Hub) register(owner string, input bool, open func(service.RunOptions) (*service.Session, error)) (*live, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, domain.Errorf(domain.ErrProviderUnavailable, "daemon is shutting down")
	}
	if h.PerClient > 0 {
		n := 0
		for _, l := range h.sessions {
			if l.owner == owner {
				n++
			}
		}
		if n >= h.PerClient {
			h.mu.Unlock()
			return nil, domain.Errorf(domain.ErrPermissionDenied, "client %s already has %d open sessions; close one first", owner, n)
		}
	}
	h.mu.Unlock()
	l := &live{
		hub: h, owner: owner, subs: map[int]chan domain.Event{}, pending: map[string]chan domain.ApprovalDecision{},
		requests: map[string]domain.ApprovalRequest{}, lastUsed: h.now(), closedSig: make(chan struct{}),
		inputs: map[string]domain.InputRequest{}, inputWait: map[string]chan domain.InputResponse{},
	}
	ro := service.RunOptions{Sink: l, Approver: l, Client: clientTag(owner)}
	if input {
		ro.Elicitor = l
	}
	sess, err := open(ro)
	if err != nil {
		return nil, err
	}
	l.sess = sess
	l.mu.Lock()
	l.id = sess.Record().ID
	l.mu.Unlock()
	h.mu.Lock()
	h.sessions[l.id] = l
	h.mu.Unlock()
	return l, nil
}

// Open starts a new session.
func (h *Hub) Open(ctx context.Context, owner string, req OpenRequest) (domain.SessionRecord, error) {
	l, err := h.register(owner, req.Input, func(ro service.RunOptions) (*service.Session, error) {
		ro.Model, ro.Mode = req.Model, req.Mode
		return h.svc.OpenSession(ctx, req.Task, ro)
	})
	if err != nil {
		return domain.SessionRecord{}, err
	}
	return l.sess.Record(), nil
}

// Resume reopens a stored session owned by the client.
func (h *Hub) Resume(ctx context.Context, owner, id, model string, input bool) (domain.SessionRecord, error) {
	if _, err := h.get(owner, id); err == nil {
		return domain.SessionRecord{}, domain.Errorf(domain.ErrInvalidInput, "session %s is already open", id)
	}
	rec, err := h.svc.SessionRecord(ctx, id)
	if err != nil || rec.Client != clientTag(owner) {
		return domain.SessionRecord{}, domain.Errorf(domain.ErrSessionNotFound, "no session %q", id)
	}
	l, err := h.register(owner, input, func(ro service.RunOptions) (*service.Session, error) {
		ro.Model = model
		return h.svc.ResumeSession(ctx, id, ro)
	})
	if err != nil {
		return domain.SessionRecord{}, err
	}
	return l.sess.Record(), nil
}

func (h *Hub) get(owner, id string) (*live, error) {
	h.mu.Lock()
	l := h.sessions[id]
	h.mu.Unlock()
	if l == nil || l.owner != owner {
		return nil, domain.Errorf(domain.ErrSessionNotFound, "no open session %q", id)
	}
	return l, nil
}

// Prompt runs one turn and waits for it. The turn keeps running if the
// caller's context ends only when detach is set.
func (h *Hub) Prompt(ctx context.Context, owner, id string, prompt []domain.ContentBlock) (domain.Result, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return domain.Result{}, err
	}
	l.mu.Lock()
	if l.busy {
		l.mu.Unlock()
		return domain.Result{}, domain.Errorf(domain.ErrInvalidInput, "a turn is already running in session %s", id)
	}
	tctx, cancel := context.WithCancel(ctx)
	l.busy, l.cancel = true, cancel
	l.lastUsed = h.now()
	l.mu.Unlock()
	defer func() {
		cancel()
		l.mu.Lock()
		l.busy, l.cancel = false, nil
		l.lastUsed = h.now()
		l.mu.Unlock()
	}()
	return l.sess.Prompt(tctx, prompt)
}

// Start runs a turn in the background; its outcome arrives as events.
func (h *Hub) Start(owner, id string, prompt []domain.ContentBlock) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	busy := l.busy
	l.mu.Unlock()
	if busy {
		return domain.Errorf(domain.ErrInvalidInput, "a turn is already running in session %s", id)
	}
	go func() { _, _ = h.Prompt(context.Background(), owner, id, prompt) }()
	return nil
}

// Cancel stops the running turn.
func (h *Hub) Cancel(ctx context.Context, owner, id string) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	l.touch()
	return l.sess.Cancel(ctx)
}

// Answer resolves a pending approval.
func (h *Hub) Answer(owner, id, approvalID string, allow bool, optionID string, remember bool) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	l.mu.Lock()
	ch := l.pending[approvalID]
	req, ok := l.requests[approvalID]
	l.mu.Unlock()
	if ch == nil || !ok {
		return domain.Errorf(domain.ErrInvalidInput, "approval %q is not waiting for an answer", approvalID)
	}
	var d domain.ApprovalDecision
	switch {
	case optionID != "":
		d = domain.ApprovalDecision{OptionID: optionID, By: "user"}
		for _, o := range req.Options {
			if o.ID == optionID {
				d.Allow = strings.HasPrefix(o.Kind, "allow")
			}
		}
		if !d.Allow && !optionExists(req, optionID) {
			return domain.Errorf(domain.ErrInvalidInput, "approval %q has no option %q", approvalID, optionID)
		}
	case allow:
		d = req.AllowOnce("user")
	default:
		d = req.RejectOnce("user")
		d.Reason = "denied by the client"
	}
	d.Remember = remember && d.Allow
	select {
	case ch <- d:
		return nil
	default:
		return domain.Errorf(domain.ErrInvalidInput, "approval %q was already answered", approvalID)
	}
}

// AnswerInput resolves a pending question from the runtime.
func (h *Hub) AnswerInput(owner, id, inputID, action string, content map[string]any) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	switch action {
	case "accept", "decline", "cancel":
	default:
		return domain.Errorf(domain.ErrInvalidInput, "action must be accept, decline or cancel")
	}
	l.touch()
	l.mu.Lock()
	ch := l.inputWait[inputID]
	l.mu.Unlock()
	if ch == nil {
		return domain.Errorf(domain.ErrInvalidInput, "input %q is not waiting for an answer", inputID)
	}
	select {
	case ch <- domain.InputResponse{Action: action, Content: content}:
		return nil
	default:
		return domain.Errorf(domain.ErrInvalidInput, "input %q was already answered", inputID)
	}
}

// PendingInputs lists questions waiting for an answer.
func (h *Hub) PendingInputs(owner, id string) ([]domain.InputRequest, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]domain.InputRequest, 0, len(l.inputs))
	for _, r := range l.inputs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ModeState is the reply to a modes query.
type ModeState struct {
	Current string        `json:"current"`
	Modes   []domain.Mode `json:"modes"`
}

// Modes returns a session's runtime modes.
func (h *Hub) Modes(owner, id string) (ModeState, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return ModeState{}, err
	}
	cur, modes, err := l.sess.Modes()
	if modes == nil {
		modes = []domain.Mode{}
	}
	return ModeState{Current: cur, Modes: modes}, err
}

// SetMode switches a session's runtime mode.
func (h *Hub) SetMode(ctx context.Context, owner, id, mode string) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	l.touch()
	return l.sess.SetMode(ctx, mode)
}

// Options returns a session's runtime settings.
func (h *Hub) Options(owner, id string) ([]domain.ConfigOption, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return nil, err
	}
	opts, err := l.sess.ConfigOptions()
	if opts == nil && err == nil {
		opts = []domain.ConfigOption{}
	}
	return opts, err
}

// SetOption changes one runtime setting of a session.
func (h *Hub) SetOption(ctx context.Context, owner, id, option, value string) ([]domain.ConfigOption, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return nil, err
	}
	l.touch()
	if err := l.sess.SetConfigOption(ctx, option, value); err != nil {
		return nil, err
	}
	return h.Options(owner, id)
}

func optionExists(r domain.ApprovalRequest, id string) bool {
	for _, o := range r.Options {
		if o.ID == id {
			return true
		}
	}
	return false
}

// Subscribe returns buffered events after seq and a channel of new ones.
func (h *Hub) Subscribe(owner, id string, after int64) ([]domain.Event, <-chan domain.Event, func(), error) {
	l, err := h.get(owner, id)
	if err != nil {
		return nil, nil, nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	var replay []domain.Event
	for _, e := range l.events {
		if e.Seq > after {
			replay = append(replay, e)
		}
	}
	ch := make(chan domain.Event, 256)
	if l.closed {
		close(ch)
		return replay, ch, func() {}, nil
	}
	sub := l.nextSub
	l.nextSub++
	l.subs[sub] = ch
	return replay, ch, func() {
		l.mu.Lock()
		if c, ok := l.subs[sub]; ok {
			close(c)
			delete(l.subs, sub)
		}
		l.mu.Unlock()
	}, nil
}

// Pending lists approvals waiting for an answer.
func (h *Hub) Pending(owner, id string) ([]domain.ApprovalRequest, error) {
	l, err := h.get(owner, id)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]domain.ApprovalRequest, 0, len(l.requests))
	for _, r := range l.requests {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Get returns the live record of an open session or the stored one.
func (h *Hub) Get(ctx context.Context, owner, id string) (domain.SessionRecord, error) {
	if l, err := h.get(owner, id); err == nil {
		return l.sess.Record(), nil
	}
	rec, err := h.svc.SessionRecord(ctx, id)
	if err != nil || rec.Client != clientTag(owner) {
		return domain.SessionRecord{}, domain.Errorf(domain.ErrSessionNotFound, "no session %q", id)
	}
	return rec, nil
}

// List returns the client's sessions, open or stored, newest first.
func (h *Hub) List(ctx context.Context, owner string) ([]domain.SessionRecord, error) {
	all, err := h.svc.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	out := []domain.SessionRecord{}
	for _, r := range all {
		if r.Client == clientTag(owner) {
			if l, err := h.get(owner, r.ID); err == nil {
				r = l.sess.Record()
			}
			out = append(out, r)
		}
	}
	return out, nil
}

// Close ends a session.
func (h *Hub) Close(ctx context.Context, owner, id string) error {
	l, err := h.get(owner, id)
	if err != nil {
		return err
	}
	return h.close(ctx, l)
}

func (h *Hub) close(ctx context.Context, l *live) error {
	h.mu.Lock()
	delete(h.sessions, l.id)
	h.mu.Unlock()
	l.mu.Lock()
	if l.cancel != nil {
		l.cancel()
	}
	l.mu.Unlock()
	err := l.sess.Close(ctx)
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		close(l.closedSig)
		for id, ch := range l.subs {
			close(ch)
			delete(l.subs, id)
		}
	}
	l.mu.Unlock()
	return err
}

// Reap closes sessions idle for longer than the idle timeout.
func (h *Hub) Reap(ctx context.Context) int {
	if h.idleTimeout <= 0 {
		return 0
	}
	h.mu.Lock()
	var idle []*live
	for _, l := range h.sessions {
		l.mu.Lock()
		if !l.busy && h.now().Sub(l.lastUsed) > h.idleTimeout && len(l.subs) == 0 {
			idle = append(idle, l)
		}
		l.mu.Unlock()
	}
	h.mu.Unlock()
	for _, l := range idle {
		_ = h.close(ctx, l)
	}
	return len(idle)
}

// Shutdown closes every session and refuses new ones.
func (h *Hub) Shutdown(ctx context.Context) {
	h.mu.Lock()
	h.closed = true
	var all []*live
	for _, l := range h.sessions {
		all = append(all, l)
	}
	h.mu.Unlock()
	var wg sync.WaitGroup
	for _, l := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = h.close(ctx, l)
		}()
	}
	wg.Wait()
}

// Open count, for status.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.sessions)
}
