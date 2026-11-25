package service

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/audit"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/policy"
	"github.com/runtime-bridge/rbridge/internal/redact"
)

// profile returns a built-in or the configured custom profile.
func (s *Service) profile(name string) (policy.Profile, error) {
	if name == "" {
		name = s.cfg.Policy.DefaultProfile
	}
	if name == "custom" {
		c := s.cfg.Policy.Custom
		p, err := policy.Custom(c.Rules, c.OutsideRead, c.ClientFSWrite, c.BlockNetwork)
		if err != nil {
			return p, domain.Errorf(domain.ErrInvalidInput, "%v", err)
		}
		return p, nil
	}
	p, ok := policy.Builtin(name)
	if !ok {
		return p, domain.Errorf(domain.ErrInvalidInput, "unknown profile %q (observe, standard, controlled, custom)", name)
	}
	return p, nil
}

// gate applies policy to every permission request and file operation of a
// session, asks the caller only when the policy says ask, and keeps the
// decisions for the audit log.
type gate struct {
	profile policy.Profile
	pctx    policy.Context
	user    domain.Approver
	timeout time.Duration
	sink    domain.EventSink
	redact  *redact.Redactor

	mu         sync.Mutex
	approvals  []audit.Approval
	lastEdit   time.Time
	remembered map[string]bool
	// approvedCmds are commands the caller allowed through a permission
	// request, so the terminal that runs them is not asked about again.
	approvedCmds map[string]time.Time
}

func commandText(req domain.ApprovalRequest) string {
	var in struct {
		Command any `json:"command"`
	}
	_ = json.Unmarshal(req.Input, &in)
	switch c := in.Command.(type) {
	case string:
		return strings.TrimSpace(c)
	case []any:
		parts := make([]string, 0, len(c))
		for _, x := range c {
			parts = append(parts, fmt.Sprint(x))
		}
		return strings.Join(parts, " ")
	}
	return ""
}

func rememberKey(r domain.ApprovalRequest) string { return r.Tool + "|" + r.Kind }

func (g *gate) record(req domain.ApprovalRequest, d domain.ApprovalDecision) {
	decision := "deny"
	if d.Allow {
		decision = "allow"
	}
	g.mu.Lock()
	g.approvals = append(g.approvals, audit.Approval{Kind: req.Kind, Tool: req.Tool, Decision: decision, By: d.By, Reason: g.redact.String(d.Reason), TimedOut: d.By == "timeout"})
	if d.Allow && req.Kind == "edit" {
		g.lastEdit = time.Now()
	}
	if cmd := commandText(req); d.Allow && req.Kind == "execute" && cmd != "" && !strings.HasPrefix(req.Tool, "terminal") {
		if g.approvedCmds == nil {
			g.approvedCmds = map[string]time.Time{}
		}
		g.approvedCmds[cmd] = time.Now()
	}
	if d.Allow && d.Remember && d.By == "user" {
		if g.remembered == nil {
			g.remembered = map[string]bool{}
		}
		g.remembered[rememberKey(req)] = true
	}
	g.mu.Unlock()
}

// Decide implements domain.Approver.
func (g *gate) Decide(ctx context.Context, req domain.ApprovalRequest) domain.ApprovalDecision {
	action, why := g.profile.Decide(req, g.pctx)
	var d domain.ApprovalDecision
	switch action {
	case policy.Allow:
		d = req.AllowOnce("policy")
		d.Reason = why
	case policy.Deny:
		d = req.RejectOnce("policy")
		d.Reason = why
	default:
		g.mu.Lock()
		again := g.remembered[rememberKey(req)]
		g.mu.Unlock()
		if again {
			d = req.AllowOnce("session")
			d.Reason = "allowed for the rest of this session"
		} else {
			d = g.ask(ctx, req)
		}
	}
	g.record(req, d)
	g.emit(domain.NewEvent(domain.EventApprovalResolved, domain.ApprovalResolvedData{ID: req.ID, Decision: d}))
	return d
}

func (g *gate) emit(e domain.Event) {
	if g.sink != nil {
		g.sink.Emit(e)
	}
}

func (g *gate) ask(ctx context.Context, req domain.ApprovalRequest) domain.ApprovalDecision {
	if g.user == nil {
		d := req.RejectOnce("policy")
		d.Reason = "needs approval and no approver is configured"
		return d
	}
	// approval.required means someone must answer; policy decisions only get approval.resolved.
	g.emit(domain.NewEvent(domain.EventApprovalRequired, req))
	actx := ctx
	if g.timeout > 0 {
		var cancel context.CancelFunc
		actx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	d := g.user.Decide(actx, req)
	if actx.Err() != nil && ctx.Err() == nil {
		d = req.RejectOnce("timeout")
		d.Reason = fmt.Sprintf("no answer within %s", g.timeout)
		g.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "warn", Message: "approval timed out and was denied: " + req.Title}))
	}
	if d.By == "" {
		d.By = "user"
	}
	// Choosing an allow_always option means the same as Remember.
	for _, o := range req.Options {
		if o.ID == d.OptionID && o.Kind == "allow_always" && d.Allow {
			d.Remember = true
		}
	}
	return d
}

// FS checks a file operation rbridge performs on behalf of an ACP agent.
func (g *gate) FS(ctx context.Context, kind, path string) error {
	input, _ := json.Marshal(map[string]string{"path": path})
	req := domain.ApprovalRequest{
		ID: "fs-" + kind, Tool: "fs/" + kind, Kind: kind, Title: kind + " " + path, Input: input,
		Options: []domain.ApprovalOption{{ID: "allow", Kind: "allow_once"}, {ID: "deny", Kind: "reject_once"}},
	}
	action, why := g.profile.Decide(req, g.pctx)
	if action == policy.Ask && kind == "edit" {
		// The agent usually asked permission for the edit just before writing.
		g.mu.Lock()
		recent := time.Since(g.lastEdit) < 2*time.Minute
		g.mu.Unlock()
		if recent {
			return nil
		}
	}
	switch action {
	case policy.Allow:
		return nil
	case policy.Deny:
		g.record(req, domain.ApprovalDecision{By: "policy", Reason: why})
		return fmt.Errorf("denied by policy: %s", why)
	}
	d := g.ask(ctx, req)
	g.record(req, d)
	g.emit(domain.NewEvent(domain.EventApprovalResolved, domain.ApprovalResolvedData{ID: req.ID, Decision: d}))
	if !d.Allow {
		return fmt.Errorf("denied: %s", d.Reason)
	}
	return nil
}

// Command checks a command an ACP agent asks rbridge to run in a terminal.
func (g *gate) Command(ctx context.Context, command string, args []string) error {
	full := strings.TrimSpace(command + " " + strings.Join(args, " "))
	input, _ := json.Marshal(map[string]string{"command": full})
	req := domain.ApprovalRequest{
		ID: "terminal", Tool: "terminal", Kind: "execute", Title: "run " + full, Input: input,
		Options: []domain.ApprovalOption{{ID: "allow", Kind: "allow_once"}, {ID: "allow-always", Kind: "allow_always"}, {ID: "deny", Kind: "reject_once"}},
	}
	action, why := g.profile.Decide(req, g.pctx)
	var d domain.ApprovalDecision
	switch action {
	case policy.Allow:
		return nil
	case policy.Deny:
		d = req.RejectOnce("policy")
		d.Reason = why
	default:
		g.mu.Lock()
		at, approved := time.Time{}, false
		for c, when := range g.approvedCmds {
			// sh -c "<cmd>" arrives as the approved text inside a longer line.
			if c == full || strings.HasSuffix(full, " "+c) {
				at, approved = when, true
			}
		}
		again := g.remembered[rememberKey(req)]
		g.mu.Unlock()
		switch {
		case approved && time.Since(at) < 2*time.Minute:
			return nil
		case again:
			d = req.AllowOnce("session")
			d.Reason = "allowed for the rest of this session"
		default:
			d = g.ask(ctx, req)
		}
	}
	g.record(req, d)
	g.emit(domain.NewEvent(domain.EventApprovalResolved, domain.ApprovalResolvedData{ID: req.ID, Decision: d}))
	if !d.Allow {
		if d.By == "policy" {
			return fmt.Errorf("denied by policy: %s", d.Reason)
		}
		return fmt.Errorf("denied: %s", d.Reason)
	}
	return nil
}

func (g *gate) snapshot() []audit.Approval {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.approvals)
}

func enforcementNotice(p policy.Profile, controls []policy.Control) domain.NoticeData {
	var enforced, runtime, not []string
	for _, c := range controls {
		switch c.Status {
		case "enforced":
			enforced = append(enforced, c.Name)
		case "runtime":
			runtime = append(runtime, c.Name)
		default:
			not = append(not, c.Name)
		}
	}
	list := func(names []string) string {
		if len(names) == 0 {
			return "none"
		}
		return strings.Join(names, ", ")
	}
	msg := fmt.Sprintf("profile %s: enforced by rbridge: %s; relies on the runtime asking: %s; not enforced: %s",
		p.Name, list(enforced), list(runtime), list(not))
	return domain.NoticeData{Message: msg, Level: "info"}
}
