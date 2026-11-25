//go:build unix

package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
)

func TestFallback(t *testing.T) {
	ctx := context.Background()
	setup := func(fallback string, change bool, classes map[string]domain.AuthMode) (*runHarness, *Service) {
		h := newRunHarness(t)
		h.env[fakeacp.EnvScenario] = "auth" // opencode refuses to start a session
		h.cfg.Resolution.Fallback = fallback
		h.cfg.Resolution.AllowBillingClassChange = change
		for id, mode := range classes {
			h.store.Put(ctx, authCollection, id, observedAuth{Mode: mode, At: time.Now()})
		}
		return h, h.svc()
	}
	task := func(h *runHarness, provider string) domain.Task {
		return domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: provider}
	}
	same := map[string]domain.AuthMode{"opencode": domain.AuthSubscription, "claude-code": domain.AuthSubscription}

	// Denied: the named provider's failure is returned as is.
	h, svc := setup("denied", false, same)
	if _, err := svc.Run(ctx, task(h, "opencode"), RunOptions{}); domain.CodeOf(err) != domain.ErrAuthenticationRequired {
		t.Fatalf("denied: %v", err)
	}

	// Allowed, same billing class: the task moves and says so.
	h, svc = setup("allowed", false, same)
	var notices []string
	res, err := svc.Run(ctx, task(h, "opencode"), RunOptions{Sink: domain.EventSinkFunc(func(e domain.Event) {
		if e.Type == domain.EventNotice {
			notices = append(notices, string(e.Data))
		}
	})})
	if err != nil || res.Provider != "claude-code" || res.Output != "echo: x" {
		t.Fatalf("same class: %+v %v", res, err)
	}
	rec, _ := svc.SessionRecord(ctx, res.SessionID)
	if rec.Fallback != "opencode to claude-code: authentication_required" {
		t.Fatalf("record %q", rec.Fallback)
	}
	if !strings.Contains(strings.Join(notices, " "), "fell back from opencode to claude-code") {
		t.Fatalf("notices %v", notices)
	}
	recs, _ := svc.Audit().Read(time.Time{})
	if len(recs) == 0 || recs[len(recs)-1].Fallback == nil || !strings.Contains(*recs[len(recs)-1].Fallback, "opencode to claude-code") {
		t.Fatalf("audit %+v", recs)
	}

	// A different or unknown billing class blocks the move unless allowed.
	for name, classes := range map[string]map[string]domain.AuthMode{
		"different": {"opencode": domain.AuthSubscription, "claude-code": domain.AuthAPIKey},
		"unknown":   {},
	} {
		h, svc = setup("allowed", false, classes)
		_, err := svc.Run(ctx, task(h, "opencode"), RunOptions{})
		if domain.CodeOf(err) != domain.ErrAuthenticationRequired || !strings.Contains(err.Error(), "no fallback") {
			t.Fatalf("%s class: %v", name, err)
		}
		h, svc = setup("allowed", true, classes)
		if res, err := svc.Run(ctx, task(h, "opencode"), RunOptions{}); err != nil || res.Provider != "claude-code" {
			t.Fatalf("%s class with allow_billing_class_change: %+v %v", name, res, err)
		}
	}

	// A named provider that is not installed falls back the same way.
	h, svc = setup("allowed", true, nil)
	if res, err := svc.Run(ctx, task(h, "codex"), RunOptions{}); err != nil || res.Provider == "codex" {
		t.Fatalf("uninstalled: %+v %v", res, err)
	}
	// Bad input is never a reason to move.
	h, svc = setup("allowed", true, nil)
	bad := task(h, "opencode")
	bad.Skills = []string{"no-such-skill"}
	if _, err := svc.Run(ctx, bad, RunOptions{}); domain.CodeOf(err) != domain.ErrSkillNotFound {
		t.Fatalf("bad input: %v", err)
	}
}
