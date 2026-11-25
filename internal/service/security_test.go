//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
)

func TestObserveRemovesWriteToolsFromClaude(t *testing.T) {
	h := newRunHarness(t)
	argsFile := filepath.Join(h.home, "args")
	h.env[fakeclaude.EnvArgsFile] = argsFile
	h.cfg.Policy.EnvAllowlist = append(h.cfg.Policy.EnvAllowlist, fakeclaude.EnvArgsFile)
	if _, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: "observe"}, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	args := string(b)
	for _, tool := range []string{"Write", "Edit", "Bash", "WebFetch"} {
		if !strings.Contains(args, "\n"+tool+"\n") {
			t.Errorf("%s not disallowed:\n%s", tool, args)
		}
	}
}

func TestDirectWritesFollowProfile(t *testing.T) {
	cases := []struct {
		profile  string
		approver domain.Approver
		written  bool
	}{
		{"observe", nil, false},
		{"standard", nil, true},
		{"controlled", nil, false},
		{"controlled", domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision { return r.AllowOnce("user") }), true},
	}
	for _, c := range cases {
		name := c.profile
		if c.approver != nil {
			name += "-approved"
		}
		t.Run(name, func(t *testing.T) {
			h := newRunHarness(t)
			h.env[fakeacp.EnvScenario] = "writedirect"
			ws := h.workspace()
			res, err := h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "opencode", Profile: c.profile}, RunOptions{Approver: c.approver})
			if err != nil {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(ws, "direct.txt"))
			if (statErr == nil) != c.written {
				t.Fatalf("written=%v want %v (output %q)", statErr == nil, c.written, res.Output)
			}
		})
	}
}

func TestPolicyDeniesWithoutAsking(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "permission"
	asked := false
	approver := domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
		asked = true
		return r.AllowOnce("user")
	})
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: "observe"}, RunOptions{Approver: approver})
	if err != nil || res.Output != "denied" || asked {
		t.Fatalf("observe must deny edits itself: %+v %v asked=%v", res, err, asked)
	}
}

func TestApprovalTimeout(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "permission"
	h.cfg.Policy.ApprovalTimeout = 300 * time.Millisecond
	slow := domain.ApproverFunc(func(ctx context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
		<-ctx.Done()
		return r.AllowOnce("user")
	})
	start := time.Now()
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: "controlled"}, RunOptions{Approver: slow})
	if err != nil || res.Output != "denied" {
		t.Fatalf("an unanswered approval must be denied: %+v %v", res, err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not applied")
	}
}

func TestRequireEnforced(t *testing.T) {
	h := newRunHarness(t)
	h.cfg.Policy.RequireEnforced = []string{"network"}
	svc := h.svc()
	_, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: "observe"}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrPolicyUnenforceable {
		t.Fatalf("got %v", err)
	}
	if list, _ := svc.Sessions(context.Background()); len(list) != 0 {
		t.Fatal("refused task left a session record")
	}
	if _, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: "standard"}, RunOptions{}); err != nil {
		t.Fatalf("standard does not ask for network blocking: %v", err)
	}
	if _, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Profile: "yolo"}, RunOptions{}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("unknown profile: %v", err)
	}
}

func TestAuditRecord(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "permission"
	svc := h.svc()
	secretPrompt := "prompt-text-that-must-not-be-audited"
	res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text(secretPrompt), Provider: "claude-code", Profile: "observe"}, RunOptions{Client: "test"})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := svc.Audit().Read(time.Time{})
	if err != nil || len(recs) != 1 {
		t.Fatalf("%+v %v", recs, err)
	}
	r := recs[0]
	if r.SessionID != res.SessionID || r.Profile != "observe" || r.Client != "test" || len(r.Approvals) != 1 || r.Approvals[0].By != "policy" || r.Approvals[0].Decision != "deny" {
		t.Fatalf("%+v", r)
	}
	if len(r.Unenforced) != 1 || r.Unenforced[0] != "network" || r.ResultDigest == "" {
		t.Fatalf("%+v", r)
	}
	entries, _ := os.ReadDir(filepath.Join(h.paths.DataDir, "audit"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(h.paths.DataDir, "audit", e.Name()))
		if strings.Contains(string(b), secretPrompt) || strings.Contains(string(b), "notes.txt") {
			t.Fatal("audit log contains prompt text or paths from tool input")
		}
	}
	h.cfg.Audit.Enabled = false
	h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code"}, RunOptions{})
	if recs, _ := svc.Audit().Read(time.Time{}); len(recs) != 1 {
		t.Fatal("disabled audit still wrote a record")
	}
}

func TestSkillFoldersAreReadOnlyAndReadable(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	addSkill(t, svc, "security-audit", domain.TrustReviewed)
	sess, err := svc.OpenSession(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Skills: []string{"security-audit"}, Provider: "claude-code", Profile: "controlled"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	var skillDir string
	for dir := range sess.gate.pctx.Skills {
		if strings.Contains(dir, "skills-session") {
			skillDir = dir
		}
	}
	read := domain.ApprovalRequest{Kind: "read", Input: []byte(`{"file_path":"` + filepath.Join(skillDir, "SKILL.md") + `"}`), Options: []domain.ApprovalOption{{ID: "a", Kind: "allow_once"}, {ID: "d", Kind: "reject_once"}}}
	if d := sess.gate.Decide(context.Background(), read); !d.Allow || d.By != "policy" {
		t.Fatalf("reading a delivered skill should not ask: %+v", d)
	}
	edit := read
	edit.Kind = "edit"
	if d := sess.gate.Decide(context.Background(), edit); d.Allow {
		t.Fatalf("editing a delivered skill must be denied: %+v", d)
	}
	script := domain.ApprovalRequest{Kind: "execute", Input: []byte(`{"command":"sh ` + filepath.Join(skillDir, "scripts", "run.sh") + `"}`), Options: read.Options}
	if d := sess.gate.Decide(context.Background(), script); d.Allow || d.By != "policy" {
		t.Fatalf("reviewed script without an approver must be denied after asking: %+v", d)
	}
}

func TestApprovalEventsOnlyWhenAsking(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "permission"
	for _, c := range []struct {
		profile  string
		required int
	}{{"standard", 0}, {"controlled", 1}} {
		log := &eventLog{}
		approver := domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision { return r.AllowOnce("user") })
		if _, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Profile: c.profile}, RunOptions{Sink: log, Approver: approver}); err != nil {
			t.Fatal(err)
		}
		if n := len(log.of(domain.EventApprovalRequired)); n != c.required {
			t.Errorf("%s: %d approval.required events, want %d", c.profile, n, c.required)
		}
		if len(log.of(domain.EventApprovalResolved)) != 1 {
			t.Errorf("%s: want one approval.resolved", c.profile)
		}
		os.Remove(filepath.Join(h.workspace(), "notes.txt"))
	}
}

func TestDefaultModelFromConfig(t *testing.T) {
	h := newRunHarness(t)
	argsFile := filepath.Join(h.home, "args")
	h.env[fakeclaude.EnvArgsFile] = argsFile
	h.cfg.Policy.EnvAllowlist = append(h.cfg.Policy.EnvAllowlist, fakeclaude.EnvArgsFile)
	h.cfg.Providers.Settings["claude-code"] = config.ProviderEntry{Model: "haiku"}
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(b), "--model\nhaiku") {
		t.Fatalf("model not passed: %s", b)
	}
	if rec, _ := h.svc().SessionRecord(context.Background(), res.SessionID); rec.Model != "haiku" {
		t.Fatalf("record model %q", rec.Model)
	}
}
