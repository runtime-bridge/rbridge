package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/runtime-bridge/rbridge/internal/audit"
	"github.com/runtime-bridge/rbridge/internal/buildinfo"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/policy"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/providers/acp"
	"github.com/runtime-bridge/rbridge/internal/providers/claudeheadless"
	"github.com/runtime-bridge/rbridge/internal/providers/registry"
	"github.com/runtime-bridge/rbridge/internal/redact"
	"github.com/runtime-bridge/rbridge/internal/skills/deliver"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
)

const sessionCollection = "sessions"

// RunOptions are per-call settings that are not part of the task itself.
type RunOptions struct {
	// Sink receives every event with sequence numbers filled in.
	Sink domain.EventSink
	// Approver answers permission requests. Nil denies everything.
	Approver domain.Approver
	Model    string
	// Profile overrides the stored profile when resuming a session.
	Profile string
	// Client names the caller in session records, for example "cli".
	Client string
}

// Session is a live bridge session.
type Session struct {
	svc     *Service
	m       registry.Manifest
	ps      domain.ProviderSession
	sink    *sequencer
	scratch string
	release func()
	timeout time.Duration
	gate    *gate

	lastOutput string
	lastCode   domain.ErrorCode

	mu     sync.Mutex
	rec    domain.SessionRecord
	closed bool
}

// Record returns a copy of the session record.
func (s *Session) Record() domain.SessionRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec
}

// Capabilities returns what the runtime reported for this session.
func (s *Session) Capabilities() domain.CapabilitySet { return s.ps.Capabilities() }

// sequencer numbers events, fills in session fields and keeps the event log.
type sequencer struct {
	seq      atomic.Int64
	session  string
	provider string
	turn     atomic.Value
	out      domain.EventSink
	logMu    sync.Mutex
	log      *os.File
	keepRaw  bool
	now      func() time.Time
	redact   *redact.Redactor
}

func (q *sequencer) Emit(e domain.Event) {
	e.Seq = q.seq.Add(1)
	e.Time = q.now()
	e.SessionID = q.session
	e.Provider = q.provider
	if t, ok := q.turn.Load().(string); ok {
		e.TurnID = t
	}
	if q.log != nil {
		le := e
		if !q.keepRaw {
			le.Raw = nil
		}
		b, _ := json.Marshal(le)
		b = q.redact.Bytes(b)
		q.logMu.Lock()
		q.log.Write(append(b, '\n'))
		q.logMu.Unlock()
	}
	if q.out != nil {
		q.out.Emit(e)
	}
}

func newID(prefix string) string {
	var b [10]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// candidate explains why a provider was or was not chosen.
type candidate struct {
	id     string
	reason string
}

// resolve picks the provider for a task: the named one, or the first in
// preference order that is available and meets every hard requirement.
func (s *Service) resolve(ctx context.Context, t domain.Task) (registry.Manifest, ProviderStatus, error) {
	reqs := slices.Clone(t.Requirements)
	if len(t.Skills) > 0 && !slices.Contains(reqs, domain.CapSkills) {
		reqs = append(reqs, domain.CapSkills)
	}
	check := func(m registry.Manifest) (ProviderStatus, string) {
		if m.Integration != domain.IntegrationACP && m.Integration != domain.IntegrationClaudeHeadless {
			return ProviderStatus{}, "cannot run tasks"
		}
		st, err := s.status(ctx, m, false)
		if err != nil {
			return st, "error: " + err.Error()
		}
		if st.Runtime.Health != domain.HealthAvailable {
			return st, string(st.Runtime.Health)
		}
		for _, r := range reqs {
			if c := m.Capabilities.Get(r); c.Supported != domain.SupportYes && c.Supported != domain.SupportPartial {
				return st, fmt.Sprintf("%s is %s", r, c.Supported)
			}
		}
		if len(t.Skills) > 0 && m.Skills.Strategy == "" {
			return st, "no per-session skill delivery"
		}
		return st, ""
	}

	if t.Provider != "" {
		if err := ctx.Err(); err != nil {
			return registry.Manifest{}, ProviderStatus{}, domain.Errorf(domain.ErrCancelled, "%v", err)
		}
		m, ok := s.registry.Get(t.Provider)
		if !ok {
			return m, ProviderStatus{}, domain.Errorf(domain.ErrProviderNotFound, "no provider %q", t.Provider)
		}
		st, why := check(m)
		if err := ctx.Err(); err != nil {
			return m, st, domain.Errorf(domain.ErrCancelled, "%v", err)
		}
		switch {
		case why == "":
			return m, st, nil
		case st.Runtime.Health != "" && st.Runtime.Health != domain.HealthAvailable:
			return m, st, &domain.Error{Code: domain.ErrProviderUnavailable, Message: fmt.Sprintf("%s is %s", m.ID, why), Provider: m.ID}
		default:
			return m, st, &domain.Error{Code: domain.ErrCapabilityUnavailable, Message: fmt.Sprintf("%s: %s", m.ID, why), Provider: m.ID}
		}
	}
	var tried []candidate
	for _, m := range s.registry.All() {
		st, why := check(m)
		if err := ctx.Err(); err != nil {
			return m, st, domain.Errorf(domain.ErrCancelled, "%v", err)
		}
		if why == "" {
			return m, st, nil
		}
		tried = append(tried, candidate{m.ID, why})
	}
	parts := make([]string, len(tried))
	details := map[string]string{}
	for i, c := range tried {
		parts[i] = c.id + ": " + c.reason
		details[c.id] = c.reason
	}
	return registry.Manifest{}, ProviderStatus{}, &domain.Error{Code: domain.ErrCapabilityUnavailable, Message: "no provider can run this task (" + strings.Join(parts, "; ") + ")", Details: details}
}

// OpenSession resolves a provider, delivers skills and starts the runtime.
func (s *Service) OpenSession(ctx context.Context, t domain.Task, ro RunOptions) (*Session, error) {
	if err := t.Validate(); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(t.Workspace); err != nil || !fi.IsDir() {
		return nil, domain.Errorf(domain.ErrInvalidInput, "workspace %s is not a directory", t.Workspace)
	}
	m, st, err := s.resolve(ctx, t)
	if err != nil {
		return nil, err
	}
	id := newID("ses_")
	rec := domain.SessionRecord{
		ID: id, ProviderID: m.ID, Workspace: t.Workspace, Profile: t.Profile, Model: ro.Model,
		State: domain.StateCreated, CreatedAt: s.now(), UpdatedAt: s.now(), Client: ro.Client, OwnerPID: os.Getpid(),
	}
	return s.start(ctx, m, st, rec, t, ro, "")
}

// ResumeSession reopens a stored session with its runtime.
func (s *Service) ResumeSession(ctx context.Context, id string, ro RunOptions) (*Session, error) {
	rec, err := s.SessionRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if rec.NativeID == "" {
		return nil, domain.Errorf(domain.ErrSessionNotFound, "session %s never reached the runtime", id)
	}
	m, ok := s.registry.Get(rec.ProviderID)
	if !ok {
		return nil, domain.Errorf(domain.ErrProviderNotFound, "provider %s is no longer configured", rec.ProviderID)
	}
	// Unknown capabilities are tried: the handshake decides, and the adapter
	// reports capability_unavailable if the runtime cannot resume.
	if m.Capabilities.Get(domain.CapSessionsResume).Supported == domain.SupportNo && m.Capabilities.Get(domain.CapSessionsLoad).Supported == domain.SupportNo {
		return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "%s cannot resume sessions", m.ID)
	}
	st, err := s.status(ctx, m, false)
	if err != nil {
		return nil, err
	}
	if st.Runtime.Health != domain.HealthAvailable {
		return nil, domain.Errorf(domain.ErrProviderUnavailable, "%s is %s", m.ID, st.Runtime.Health)
	}
	var skills []string
	for _, d := range rec.Skills {
		skills = append(skills, d.SkillID)
	}
	if ro.Model == "" {
		ro.Model = rec.Model
	}
	rec.OwnerPID = os.Getpid()
	rec.Client = ro.Client
	t := domain.Task{Workspace: rec.Workspace, Prompt: domain.Text("resume"), Skills: skills, Provider: m.ID, Profile: rec.Profile}
	if ro.Profile != "" {
		t.Profile = ro.Profile
	}
	return s.start(ctx, m, st, rec, t, ro, rec.NativeID)
}

func (s *Service) start(ctx context.Context, m registry.Manifest, st ProviderStatus, rec domain.SessionRecord, t domain.Task, ro RunOptions, resume string) (*Session, error) {
	release, err := s.acquire(ctx)
	if err != nil {
		return nil, err
	}
	scratch := filepath.Join(s.paths.RuntimeDir, "sessions", rec.ID)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		release()
		return nil, err
	}
	// Requests refused before a runtime starts (bad skills, delivery problems)
	// are not sessions and leave no record.
	persisted := false
	fail := func(err error) (*Session, error) {
		err = s.scrub(err)
		if persisted {
			rec.State, rec.LastError, rec.UpdatedAt = domain.StateFailed, err.Error(), s.now()
			_ = s.store.Put(context.Background(), sessionCollection, rec.ID, rec)
		}
		os.RemoveAll(scratch)
		release()
		return nil, err
	}

	prof, err := s.profile(t.Profile)
	if err != nil {
		return fail(err)
	}
	controls := policy.Enforcement(prof, m.Integration)
	unenforced := policy.Unenforced(prof, controls)
	for _, need := range s.cfg.Policy.RequireEnforced {
		if slices.Contains(unenforced, need) {
			return fail(domain.Errorf(domain.ErrPolicyUnenforceable, "profile %s wants %s restricted, which rbridge cannot enforce for %s", prof.Name, need, m.ID))
		}
	}
	rec.Profile = prof.Name
	rec.Unenforced = unenforced

	seq := &sequencer{session: rec.ID, provider: m.ID, out: ro.Sink, now: s.now, keepRaw: s.cfg.Audit.Level == "full", redact: s.redact}
	if s.cfg.Audit.Level == "events" || s.cfg.Audit.Level == "full" {
		dir := filepath.Join(s.paths.DataDir, "events")
		if err := os.MkdirAll(dir, 0o700); err == nil {
			if f, err := os.OpenFile(filepath.Join(dir, rec.ID+".jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				seq.log = f
			}
		}
	}

	plan, trusts, err := s.prepareSkills(ctx, m, t, scratch, seq)
	if err != nil {
		return fail(err)
	}
	rec.Skills = plan.Deliveries
	pctx := policy.Context{Workspace: t.Workspace, Skills: map[string]domain.TrustState{}}
	for id, dir := range plan.Paths {
		pctx.Skills[dir] = trusts[id].trust
		pctx.Skills[trusts[id].lib] = trusts[id].trust
	}
	g := &gate{profile: prof, pctx: pctx, user: ro.Approver, timeout: s.cfg.Policy.ApprovalTimeout, sink: seq, redact: s.redact}
	seq.Emit(domain.NewEvent(domain.EventNotice, enforcementNotice(prof, controls)))
	if st.Runtime.Changed != "" {
		seq.Emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: m.DisplayName + ": " + st.Runtime.Changed + "; run rbridge providers accept " + m.ID + " if you expected this"}))
	}

	allow := append(slices.Clone(s.cfg.Policy.EnvAllowlist), m.Launch.EnvPassthrough...)
	extra := maps.Clone(plan.Env)
	if extra == nil {
		extra = map[string]string{}
	}
	maps.Copy(extra, m.Launch.SetEnv)
	env := process.FilterEnv(s.environ(), allow, extra)
	opts := domain.SessionOptions{
		BridgeID: rec.ID, Workspace: t.Workspace, Executable: st.Runtime.Executable, Env: env,
		ResumeNativeID: resume, SkillDirs: plan.SkillDirs, MCPServers: t.MCPServers, Model: ro.Model,
		OutputSchema: t.OutputSchema, Approver: g, IdleTimeout: s.cfg.Execution.IdleTimeout,
		KillGrace: s.cfg.Execution.KillGracePeriod, ScratchDir: scratch,
		ClientFSWrite: prof.ClientFSWrite, FSGate: g.FS, DisallowedTools: prof.ClaudeDisallowed,
	}
	if opts.Model == "" {
		opts.Model = s.cfg.Providers.Settings[m.ID].Model
		rec.Model = opts.Model
	}
	rec.State = domain.StateStarting
	_ = s.store.Put(ctx, sessionCollection, rec.ID, rec)
	persisted = true

	var ps domain.ProviderSession
	switch m.Integration {
	case domain.IntegrationACP:
		ps, err = acp.Open(ctx, m.Capabilities, acp.Launch{Args: s.launchArgs(m), ClientName: "rbridge", ClientVersion: buildinfo.Get().Version}, opts, seq)
	case domain.IntegrationClaudeHeadless:
		ps, err = claudeheadless.Open(ctx, m.Capabilities, s.launchArgs(m), opts, seq)
	default:
		err = domain.Errorf(domain.ErrCapabilityUnavailable, "%s cannot run tasks", m.ID)
	}
	if err != nil {
		var de *domain.Error
		if errors.As(err, &de) && de.Provider == "" {
			de.Provider = m.ID
		}
		return fail(err)
	}
	rec.NativeID = ps.NativeID()
	rec.State = domain.StateReady
	rec.UpdatedAt = s.now()
	_ = s.store.Put(ctx, sessionCollection, rec.ID, rec)
	timeout := t.Timeout
	if timeout == 0 {
		timeout = s.cfg.Execution.TaskTimeout
	}
	return &Session{svc: s, m: m, ps: ps, sink: seq, scratch: scratch, release: release, rec: rec, timeout: timeout, gate: g}, nil
}

type skillTrust struct {
	trust domain.TrustState
	lib   string
}

func (s *Service) prepareSkills(ctx context.Context, m registry.Manifest, t domain.Task, scratch string, seq *sequencer) (deliver.Plan, map[string]skillTrust, error) {
	trusts := map[string]skillTrust{}
	if len(t.Skills) == 0 {
		return deliver.Plan{Env: map[string]string{}}, trusts, nil
	}
	reg := s.Skills()
	var items []deliver.Item
	for _, id := range t.Skills {
		rec, err := reg.Require(ctx, id)
		if err != nil {
			return deliver.Plan{}, nil, err
		}
		switch {
		case rec.Trust == domain.TrustBlocked:
			return deliver.Plan{}, nil, domain.Errorf(domain.ErrSkillBlocked, "skill %s is blocked", id)
		case rec.Trust == domain.TrustUnreviewed && !s.cfg.Skills.AllowUnreviewed:
			return deliver.Plan{}, nil, domain.Errorf(domain.ErrSkillBlocked, "skill %s is unreviewed; review it and run rbridge skills trust %s reviewed", id, id)
		}
		if err := reg.Verify(ctx, rec); err != nil {
			return deliver.Plan{}, nil, err
		}
		dir := reg.Path(rec)
		sk, err := spec.Parse(dir, reg.Limits)
		if err != nil {
			return deliver.Plan{}, nil, &domain.Error{Code: domain.ErrSkillInvalid, Message: err.Error(), Cause: err}
		}
		items = append(items, deliver.Item{Record: rec, Dir: dir, Scripts: rec.Trust.ScriptsAllowed(), ScriptFiles: sk.Scripts})
		trusts[id] = skillTrust{trust: rec.Trust, lib: dir}
	}
	plan, err := deliver.Prepare(deliver.Request{
		Strategy: deliver.Strategy(m.Skills.Strategy), Layout: m.Skills.SessionDir, Scratch: scratch, Items: items, Getenv: s.getenv,
	})
	if err != nil {
		return plan, nil, err
	}
	loc := s.skillLocations(m)
	for _, c := range deliver.FindClashes(t.Skills, t.Workspace, loc.Project, loc.User) {
		seq.Emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: fmt.Sprintf("another skill named %s exists at %s; %s may use that copy instead", c.SkillID, c.Path, m.DisplayName)}))
	}
	for _, d := range plan.Deliveries {
		seq.Emit(domain.NewEvent(domain.EventSkillDelivered, domain.SkillDeliveredData{SkillID: d.SkillID, Digest: d.Digest, Mode: d.Mode, Scripts: plan.Scripts[d.SkillID]}))
	}
	return plan, trusts, nil
}

// Prompt runs one turn within the task timeout.
func (s *Session) Prompt(ctx context.Context, prompt []domain.ContentBlock) (domain.Result, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.Result{}, domain.Errorf(domain.ErrSessionNotFound, "session is closed")
	}
	s.rec.Turns++
	turnID := fmt.Sprintf("%s.%d", s.rec.ID, s.rec.Turns)
	s.rec.State = domain.StateRunning
	s.rec.UpdatedAt = s.svc.now()
	rec := s.rec
	s.mu.Unlock()
	_ = s.svc.store.Put(ctx, sessionCollection, rec.ID, rec)

	s.sink.turn.Store(turnID)
	s.sink.Emit(domain.NewEvent(domain.EventTurnStarted, nil))
	tctx := ctx
	var cancel context.CancelFunc
	if s.timeout > 0 {
		tctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	res, err := s.ps.Prompt(tctx, domain.Turn{ID: turnID, Prompt: prompt})
	if err == nil && res.StopReason == domain.StopCancelled && ctx.Err() == nil && tctx.Err() != nil {
		err = domain.Errorf(domain.ErrExecutionTimeout, "task timed out after %s", s.timeout)
	}

	s.mu.Lock()
	s.rec.NativeID = s.ps.NativeID()
	s.rec.UpdatedAt = s.svc.now()
	if err != nil {
		err = s.svc.scrub(err)
		s.rec.State = domain.StateReady
		s.rec.LastError = err.Error()
	} else {
		s.rec.State = domain.StateReady
		s.rec.LastStop = res.StopReason
		s.rec.LastError = ""
	}
	rec = s.rec
	s.mu.Unlock()
	_ = s.svc.store.Put(context.Background(), sessionCollection, rec.ID, rec)

	if err != nil {
		var de *domain.Error
		if !errors.As(err, &de) {
			de = &domain.Error{Code: domain.ErrExecutionFailed, Message: err.Error(), Cause: err}
		}
		if de.Provider == "" {
			de.Provider = s.m.ID
		}
		de.SessionID = rec.ID
		s.mu.Lock()
		s.lastCode = de.Code
		s.mu.Unlock()
		s.sink.Emit(domain.NewEvent(domain.EventTurnFailed, domain.TurnFailedData{Code: de.Code, Message: de.Message}))
		return domain.Result{SessionID: rec.ID, Provider: s.m.ID, Skills: rec.Skills, Errors: []*domain.Error{de}}, de
	}
	s.mu.Lock()
	s.lastOutput, s.lastCode = res.Output, ""
	s.mu.Unlock()
	if res.AuthMode != "" && res.AuthMode != domain.AuthUnknown {
		_ = s.svc.store.Put(context.Background(), authCollection, s.m.ID, observedAuth{Mode: res.AuthMode, At: s.svc.now()})
	}
	s.sink.Emit(domain.NewEvent(domain.EventTurnCompleted, domain.TurnCompletedData{StopReason: res.StopReason, Output: res.Output, Structured: res.Structured, Usage: res.Usage}))
	return domain.Result{
		SessionID: rec.ID, StopReason: res.StopReason, Output: res.Output, Structured: res.Structured,
		Usage: res.Usage, Provider: s.m.ID, Skills: rec.Skills,
	}, nil
}

// Cancel stops the current turn.
func (s *Session) Cancel(ctx context.Context) error { return s.ps.Cancel(ctx) }

// Close ends the session, stops the runtime and removes delivered skills.
func (s *Session) Close(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	err := s.ps.Close(ctx)
	s.sink.turn.Store("")
	s.sink.Emit(domain.NewEvent(domain.EventSessionClosed, nil))
	if s.sink.log != nil {
		s.sink.log.Close()
	}
	os.RemoveAll(s.scratch)
	s.mu.Lock()
	s.rec.State = domain.StateClosed
	s.rec.UpdatedAt = s.svc.now()
	rec := s.rec
	s.mu.Unlock()
	_ = s.svc.store.Put(context.Background(), sessionCollection, rec.ID, rec)
	s.svc.writeAudit(rec, s)
	s.release()
	return err
}

func (s *Service) writeAudit(rec domain.SessionRecord, sess *Session) {
	if !s.cfg.Audit.Enabled {
		return
	}
	sess.mu.Lock()
	out, code := sess.lastOutput, sess.lastCode
	sess.mu.Unlock()
	r := audit.Record{
		ID: newID("aud_"), Time: s.now().UTC(), SessionID: rec.ID, Client: rec.Client, Provider: rec.ProviderID,
		Workspace: rec.Workspace, Profile: rec.Profile, Skills: rec.Skills, Approvals: sess.gate.snapshot(),
		Turns: rec.Turns, StopReason: rec.LastStop, ErrorCode: code, Unenforced: rec.Unenforced,
	}
	if out != "" {
		sum := sha256.Sum256([]byte(out))
		r.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	_ = s.Audit().Append(r)
}

// Audit returns the audit log.
func (s *Service) Audit() *audit.Log {
	return &audit.Log{Dir: filepath.Join(s.paths.DataDir, "audit"), Retention: s.cfg.Audit.Retention, Now: s.now}
}

// Run executes a task as a one-turn session.
func (s *Service) Run(ctx context.Context, t domain.Task, ro RunOptions) (domain.Result, error) {
	sess, err := s.OpenSession(ctx, t, ro)
	if err != nil {
		return domain.Result{}, err
	}
	defer sess.Close(context.Background())
	return sess.Prompt(ctx, t.Prompt)
}

func (s *Service) acquire(ctx context.Context) (func(), error) {
	wait := s.cfg.Execution.SlotWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case s.slots <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-s.slots }) }, nil
	case <-ctx.Done():
		return nil, domain.Errorf(domain.ErrCancelled, "waiting for a free session slot: %v", ctx.Err())
	case <-timer.C:
		return nil, &domain.Error{Code: domain.ErrProviderUnavailable, Retryable: true,
			Message: fmt.Sprintf("all %d session slots are busy (execution.max_concurrent_sessions); try again later", cap(s.slots))}
	}
}

// SessionRecord returns a stored session, marking it interrupted when its
// owning process is gone.
func (s *Service) SessionRecord(ctx context.Context, id string) (domain.SessionRecord, error) {
	var rec domain.SessionRecord
	ok, err := s.store.Get(ctx, sessionCollection, id, &rec)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, domain.Errorf(domain.ErrSessionNotFound, "no session %q", id)
	}
	return s.reconcile(ctx, rec), nil
}

// Sessions lists stored sessions, newest first.
func (s *Service) Sessions(ctx context.Context) ([]domain.SessionRecord, error) {
	keys, err := s.store.List(ctx, sessionCollection)
	if err != nil {
		return nil, err
	}
	var out []domain.SessionRecord
	for _, k := range keys {
		var rec domain.SessionRecord
		if ok, _ := s.store.Get(ctx, sessionCollection, k, &rec); ok {
			out = append(out, s.reconcile(ctx, rec))
		}
	}
	slices.SortFunc(out, func(a, b domain.SessionRecord) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

func (s *Service) live(rec domain.SessionRecord) bool {
	if rec.OwnerPID == 0 {
		return false
	}
	if _, err := os.Stat(filepath.Join(s.paths.RuntimeDir, "sessions", rec.ID)); err != nil {
		return false
	}
	return processAlive(rec.OwnerPID)
}

func (s *Service) reconcile(ctx context.Context, rec domain.SessionRecord) domain.SessionRecord {
	switch rec.State {
	case domain.StateClosed, domain.StateFailed, domain.StateInterrupted:
		return rec
	}
	if rec.OwnerPID == os.Getpid() || s.live(rec) {
		return rec
	}
	rec.State = domain.StateInterrupted
	_ = s.store.Put(ctx, sessionCollection, rec.ID, rec)
	return rec
}

// CancelSession asks the rbridge process that owns a session to cancel it.
func (s *Service) CancelSession(ctx context.Context, id string) error {
	rec, err := s.SessionRecord(ctx, id)
	if err != nil {
		return err
	}
	if !s.live(rec) {
		return domain.Errorf(domain.ErrSessionNotFound, "session %s is not running (state %s)", id, rec.State)
	}
	return interruptProcess(rec.OwnerPID)
}

// DeliveryFor returns a provider's per-session skill delivery strategy and layout.
func (s *Service) DeliveryFor(id string) (string, string) {
	m, ok := s.registry.Get(id)
	if !ok {
		return "", ""
	}
	return m.Skills.Strategy, m.Skills.SessionDir
}

// launchArgs returns the configured override or the manifest's arguments.
func (s *Service) launchArgs(m registry.Manifest) []string {
	if a := s.cfg.Providers.Settings[m.ID].Args; a != nil {
		return a
	}
	return m.Launch.Args
}

// scrub masks secrets in an error's message and details. Runtime stderr ends
// up in details and may contain keys.
func (s *Service) scrub(err error) error {
	var de *domain.Error
	if !errors.As(err, &de) {
		return err
	}
	de.Message = s.redact.String(de.Message)
	s.redact.Map(de.Details)
	return de
}

const authCollection = "auth"

// observedAuth is the sign-in mode a runtime revealed during a session.
type observedAuth struct {
	Mode domain.AuthMode `json:"mode"`
	At   time.Time       `json:"at"`
}

// PruneSessions deletes finished sessions last updated before the cutoff,
// with their event logs. Open sessions are never pruned.
func (s *Service) PruneSessions(ctx context.Context, olderThan time.Duration) (int, error) {
	if olderThan <= 0 {
		return 0, domain.Errorf(domain.ErrInvalidInput, "age must be positive")
	}
	list, err := s.Sessions(ctx)
	if err != nil {
		return 0, err
	}
	cutoff := s.now().Add(-olderThan)
	n := 0
	for _, r := range list {
		switch r.State {
		case domain.StateClosed, domain.StateFailed, domain.StateInterrupted:
		default:
			continue
		}
		if r.UpdatedAt.After(cutoff) {
			continue
		}
		if err := s.store.Delete(ctx, sessionCollection, r.ID); err != nil {
			return n, err
		}
		os.Remove(filepath.Join(s.paths.DataDir, "events", r.ID+".jsonl"))
		n++
	}
	return n, nil
}
