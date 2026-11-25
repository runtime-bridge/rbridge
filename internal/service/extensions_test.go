//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/policy"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
)

func TestTerminalFollowsPolicy(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "terminal"
	ctx := context.Background()
	run := func(profile, cmd string, approver domain.Approver) (string, []domain.Event) {
		t.Helper()
		var events []domain.Event
		task := domain.Task{Workspace: h.workspace(), Prompt: domain.Text(cmd), Provider: "opencode", Profile: profile}
		res, err := h.svc().Run(ctx, task, RunOptions{Approver: approver, Sink: domain.EventSinkFunc(func(e domain.Event) { events = append(events, e) })})
		if err != nil {
			t.Fatal(err)
		}
		return res.Output, events
	}
	if out, _ := run("observe", "echo observed", nil); !strings.Contains(out, "does not offer terminals") {
		t.Fatalf("observe is read-only: %q", out)
	}
	h.cfg.Policy.Custom.Rules = map[string]string{"execute": "allow"}
	if out, _ := run("custom", "echo allowed", nil); !strings.HasPrefix(out, "exit=0 allowed") {
		t.Fatalf("custom allow: %q", out)
	}
	if out, _ := run("controlled", "echo no", nil); !strings.Contains(out, "no approver") {
		t.Fatalf("controlled without approver: %q", out)
	}
	asked := 0
	yes := domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
		asked++
		if r.Tool != "terminal" || r.Kind != "execute" || !strings.Contains(r.Title, "echo yes") {
			t.Errorf("request %+v", r)
		}
		return r.AllowOnce("user")
	})
	if out, events := run("controlled", "echo yes", yes); !strings.HasPrefix(out, "exit=0 yes") || asked != 1 {
		t.Fatalf("controlled with approval: %q asked %d", out, asked)
	} else {
		found := false
		for _, e := range events {
			found = found || (e.Type == domain.EventNotice && strings.Contains(string(e.Data), "client terminals"))
		}
		if !found {
			t.Fatal("enforcement report must list client terminals")
		}
	}
	h.cfg.Policy.Custom.Rules = map[string]string{"execute": "deny"}
	if out, _ := run("custom", "echo denied", nil); !strings.Contains(out, "does not offer terminals") {
		t.Fatalf("custom deny: %q", out)
	}
}

func TestApprovedCommandIsNotAskedTwice(t *testing.T) {
	g := &gate{profile: mustProfile(t, "controlled")}
	asked := 0
	g.user = domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
		asked++
		return r.AllowOnce("user")
	})
	approve := domain.ApprovalRequest{ID: "1", Tool: "bash", Kind: "execute", Input: []byte(`{"command":"go test ./..."}`),
		Options: []domain.ApprovalOption{{ID: "allow", Kind: "allow_once"}}}
	if !g.Decide(context.Background(), approve).Allow {
		t.Fatal("approval")
	}
	if err := g.Command(context.Background(), "sh", []string{"-c", "go test ./..."}); err != nil || asked != 1 {
		t.Fatalf("same command asked again: %v asked %d", err, asked)
	}
	if err := g.Command(context.Background(), "sh", []string{"-c", "rm -rf ."}); err != nil || asked != 2 {
		t.Fatalf("a different command must be asked: %v asked %d", err, asked)
	}
}

func mustProfile(t *testing.T, name string) policy.Profile {
	t.Helper()
	p, err := (&Service{}).profile(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestModesThroughService(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	sess, err := h.svc().OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{Mode: "code"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(ctx)
	cur, modes, err := sess.Modes()
	if err != nil || cur != "code" || len(modes) != 2 {
		t.Fatalf("modes %s %v %v", cur, modes, err)
	}
	if err := sess.SetConfigOption(ctx, "model", "large"); err != nil {
		t.Fatal(err)
	}
	res, err := sess.Prompt(ctx, domain.Text("mode?"))
	if err != nil || res.Output != "mode: code model: large" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := h.svc().OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{Mode: "nope"}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("unknown mode: %v", err)
	}

	claude, err := h.svc().OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer claude.Close(ctx)
	if _, _, err := claude.Modes(); domain.CodeOf(err) != domain.ErrCapabilityUnavailable {
		t.Fatalf("claude modes: %v", err)
	}
}

func TestElicitationThroughService(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "elicit"
	ctx := context.Background()
	task := domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}
	res, err := h.svc().Run(ctx, task, RunOptions{Elicitor: domain.ElicitorFunc(func(_ context.Context, r domain.InputRequest) domain.InputResponse {
		return domain.InputResponse{Action: "accept", Content: map[string]any{"name": "Grace"}}
	})})
	if err != nil || res.Output != "hello Grace" {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ := h.svc().Run(ctx, task, RunOptions{}); res.Output != "declined" {
		t.Fatalf("no elicitor: %q", res.Output)
	}
}

func TestSymlinkedWorkspace(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "permission"
	real := h.workspace()
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: link, Prompt: domain.Text("write"), Provider: "opencode", Profile: "controlled"},
		RunOptions{Approver: domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision { return r.AllowOnce("user") })})
	if err != nil || res.Output != "allowed" {
		t.Fatalf("a workspace reached through a link must accept its own files: %+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(real, "notes.txt")); err != nil {
		t.Fatal(err)
	}
}
