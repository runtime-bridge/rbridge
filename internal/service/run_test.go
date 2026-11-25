//go:build unix

package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/process"
	"github.com/rbridge/rbridge/internal/providers/detect"
	"github.com/rbridge/rbridge/internal/providers/fakeacp"
	"github.com/rbridge/rbridge/internal/providers/fakeclaude"
	"github.com/rbridge/rbridge/internal/skills/registry"
	"github.com/rbridge/rbridge/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("RBRIDGE_FAKE_SVC") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "claude":
			fakeclaude.Main()
		case "opencode":
			fakeacp.Main()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type runHarness struct {
	t     *testing.T
	home  string
	bin   string
	env   map[string]string
	cfg   config.Config
	paths config.Paths
	store store.Store
}

func newRunHarness(t *testing.T) *runHarness {
	t.Helper()
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"claude", "opencode"} {
		if err := os.Symlink(exe, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.Policy.EnvAllowlist = append(cfg.Policy.EnvAllowlist, "RBRIDGE_FAKE_SVC", fakeacp.EnvScenario, fakeclaude.EnvScenario)
	cfg.Execution.KillGracePeriod = time.Second
	cfg.Execution.DetectCacheTTL = 0
	paths, _ := config.ResolvePaths(config.Env{GOOS: "linux", Getenv: func(k string) string {
		if k == "RBRIDGE_HOME" {
			return filepath.Join(home, "rb")
		}
		return ""
	}})
	return &runHarness{
		t: t, home: home, bin: bin, cfg: cfg, paths: paths, store: store.NewMemStore(),
		env: map[string]string{"PATH": bin + ":/usr/bin:/bin", "RBRIDGE_FAKE_SVC": "1", "HOME": home, "SECRET_TOKEN": "do-not-pass"},
	}
}

func (h *runHarness) svc() *Service {
	h.t.Helper()
	getenv := func(k string) string { return h.env[k] }
	environ := func() []string {
		var out []string
		for k, v := range h.env {
			out = append(out, k+"="+v)
		}
		return out
	}
	det := &detect.Detector{FS: detect.OSFS, Run: process.Run, Getenv: getenv, Environ: environ, GOOS: "linux", Home: h.home, EnvAllowlist: h.cfg.Policy.EnvAllowlist, Timeout: 5 * time.Second}
	s, err := New(Options{Paths: h.paths, Config: h.cfg, Store: h.store, Detector: det, Getenv: getenv, Environ: environ})
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *runHarness) workspace() string {
	ws := filepath.Join(h.home, "ws")
	os.MkdirAll(ws, 0o755)
	return ws
}

type eventLog struct {
	mu sync.Mutex
	ev []domain.Event
}

func (l *eventLog) Emit(e domain.Event) {
	l.mu.Lock()
	l.ev = append(l.ev, e)
	l.mu.Unlock()
}

func (l *eventLog) of(t domain.EventType) []domain.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []domain.Event
	for _, e := range l.ev {
		if e.Type == t {
			out = append(out, e)
		}
	}
	return out
}

func TestRunOnBothProviders(t *testing.T) {
	for _, provider := range []string{"opencode", "claude-code"} {
		t.Run(provider, func(t *testing.T) {
			h := newRunHarness(t)
			svc := h.svc()
			log := &eventLog{}
			res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("hello bridge"), Provider: provider}, RunOptions{Sink: log, Client: "test"})
			if err != nil {
				t.Fatal(err)
			}
			if res.StopReason != domain.StopEndTurn || !strings.Contains(res.Output, "hello bridge") || res.Provider != provider {
				t.Fatalf("result %+v", res)
			}
			log.mu.Lock()
			for i, e := range log.ev {
				if e.Seq != int64(i+1) || e.SessionID != res.SessionID || e.Provider != provider {
					t.Fatalf("event %d: %+v", i, e)
				}
			}
			last := log.ev[len(log.ev)-1].Type
			log.mu.Unlock()
			if last != domain.EventSessionClosed || len(log.of(domain.EventTurnCompleted)) != 1 {
				t.Fatalf("event order wrong")
			}
			rec, err := svc.SessionRecord(context.Background(), res.SessionID)
			if err != nil || rec.State != domain.StateClosed || rec.NativeID == "" || rec.Turns != 1 {
				t.Fatalf("record %+v %v", rec, err)
			}
			if _, err := os.Stat(filepath.Join(h.paths.RuntimeDir, "sessions", res.SessionID)); !os.IsNotExist(err) {
				t.Fatal("scratch directory not removed")
			}
		})
	}
}

func TestResolveByPreferenceAndRequirements(t *testing.T) {
	h := newRunHarness(t)
	ws := h.workspace()
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x")}, RunOptions{})
	if err != nil || res.Provider != "claude-code" {
		t.Fatalf("default order should pick claude-code: %+v %v", res, err)
	}
	// Only OpenCode declares partial offline support.
	res, err = h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Requirements: []domain.CapabilityName{domain.CapOffline}}, RunOptions{})
	if err != nil || res.Provider != "opencode" {
		t.Fatalf("offline requirement should pick opencode: %+v %v", res, err)
	}
	_, err = h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Requirements: []domain.CapabilityName{domain.CapPromptAudio}}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrCapabilityUnavailable || !strings.Contains(err.Error(), "prompt.audio") {
		t.Fatalf("got %v", err)
	}
	os.Remove(filepath.Join(h.bin, "opencode"))
	_, err = h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrProviderUnavailable {
		t.Fatalf("missing explicit provider: %v", err)
	}
	_, err = h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "nope"}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrProviderNotFound {
		t.Fatalf("unknown provider: %v", err)
	}
	_, err = h.svc().Run(context.Background(), domain.Task{Workspace: filepath.Join(ws, "missing"), Prompt: domain.Text("x")}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("missing workspace: %v", err)
	}
}

func TestRuntimeEnvironmentIsFiltered(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "env"
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("SECRET_TOKEN HOME"), Provider: "opencode"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "SECRET_TOKEN= ") || !strings.Contains(res.Output, "HOME="+h.home) {
		t.Fatalf("env not filtered as expected: %q", res.Output)
	}
}

func addSkill(t *testing.T, svc *Service, name string, trust domain.TrustState) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), name)
	os.MkdirAll(filepath.Join(src, "scripts"), 0o755)
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Test.\nlicense: MIT\n---\nbody\n"), 0o644)
	os.WriteFile(filepath.Join(src, "scripts", "run.sh"), []byte("echo\n"), 0o755)
	if _, err := svc.Skills().Add(context.Background(), registry.AddRequest{Path: src}); err != nil {
		t.Fatal(err)
	}
	if trust != domain.TrustUnreviewed {
		if _, err := svc.Skills().SetTrust(context.Background(), name, trust); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func TestSkillsDeliveredToClaude(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	addSkill(t, svc, "security-audit", domain.TrustTrusted)
	log := &eventLog{}
	res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("audit"), Skills: []string{"security-audit"}, Provider: "claude-code"}, RunOptions{Sink: log})
	if err != nil {
		t.Fatal(err)
	}
	var started domain.SessionStartedData
	json.Unmarshal(log.of(domain.EventSessionStarted)[0].Data, &started)
	if !slices.Contains(started.Skills, "security-audit") {
		t.Fatalf("Claude did not see the skill: %+v", started)
	}
	var d domain.SkillDeliveredData
	json.Unmarshal(log.of(domain.EventSkillDelivered)[0].Data, &d)
	if d.SkillID != "security-audit" || !d.Scripts || !strings.HasPrefix(d.Mode, "session-dir") {
		t.Fatalf("delivery %+v", d)
	}
	if len(res.Skills) != 1 {
		t.Fatalf("result skills %+v", res.Skills)
	}
}

func TestSkillsDeliveredToOpenCode(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "env"
	svc := h.svc()
	addSkill(t, svc, "code-review", domain.TrustRestricted)
	log := &eventLog{}
	res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("OPENCODE_CONFIG"), Skills: []string{"code-review"}, Provider: "opencode"}, RunOptions{Sink: log})
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := strings.TrimPrefix(strings.TrimSpace(res.Output), "OPENCODE_CONFIG=")
	if !strings.HasPrefix(cfgPath, h.paths.RuntimeDir) {
		t.Fatalf("OPENCODE_CONFIG not set to a session file: %q", res.Output)
	}
	var d domain.SkillDeliveredData
	json.Unmarshal(log.of(domain.EventSkillDelivered)[0].Data, &d)
	if d.Scripts || !strings.HasSuffix(d.Mode, "+no-scripts") {
		t.Fatalf("restricted skill must not get scripts: %+v", d)
	}
}

func TestSkillTrustGates(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	ws := h.workspace()
	addSkill(t, svc, "unreviewed-one", domain.TrustUnreviewed)
	addSkill(t, svc, "blocked-one", domain.TrustBlocked)
	src := addSkill(t, svc, "changed-one", domain.TrustTrusted)
	for _, id := range []string{"unreviewed-one", "blocked-one"} {
		_, err := svc.Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Skills: []string{id}}, RunOptions{})
		if domain.CodeOf(err) != domain.ErrSkillBlocked {
			t.Errorf("%s: %v", id, err)
		}
	}
	if _, err := svc.Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Skills: []string{"missing"}}, RunOptions{}); domain.CodeOf(err) != domain.ErrSkillNotFound {
		t.Errorf("missing: %v", err)
	}
	// A library copy changed behind rbridge's back is refused.
	rec, _ := svc.Skills().Require(context.Background(), "changed-one")
	os.WriteFile(filepath.Join(svc.Skills().Path(rec), "SKILL.md"), []byte("---\nname: changed-one\ndescription: evil\n---\n"), 0o644)
	_ = src
	if _, err := svc.Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Skills: []string{"changed-one"}}, RunOptions{}); domain.CodeOf(err) != domain.ErrSkillDigestMismatch {
		t.Errorf("changed: %v", err)
	}
	h.cfg.Skills.AllowUnreviewed = true
	if _, err := h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Skills: []string{"unreviewed-one"}}, RunOptions{}); err != nil {
		t.Errorf("allow_unreviewed should permit it: %v", err)
	}
}

func TestClashNotice(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	ws := h.workspace()
	addSkill(t, svc, "doc-writer", domain.TrustTrusted)
	os.MkdirAll(filepath.Join(ws, ".claude", "skills", "doc-writer"), 0o755)
	os.WriteFile(filepath.Join(ws, ".claude", "skills", "doc-writer", "SKILL.md"), []byte("x"), 0o644)
	log := &eventLog{}
	if _, err := svc.Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Skills: []string{"doc-writer"}, Provider: "claude-code"}, RunOptions{Sink: log}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range log.of(domain.EventNotice) {
		if strings.Contains(string(e.Data), "another skill named doc-writer") {
			found = true
		}
	}
	if !found {
		t.Fatal("clash not reported")
	}
}

func TestTaskTimeout(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "slow"
	_, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Timeout: 300 * time.Millisecond}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrExecutionTimeout {
		t.Fatalf("got %v", err)
	}
}

func TestApprovalsFlowThroughService(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "permission"
	ws := h.workspace()
	var asked []domain.ApprovalRequest
	approver := domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
		asked = append(asked, r)
		return r.AllowOnce("user")
	})
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "claude-code", Profile: "controlled"}, RunOptions{Approver: approver})
	if err != nil || res.Output != "allowed" || len(asked) != 1 || asked[0].SessionID != res.SessionID {
		t.Fatalf("%+v %v %+v", res, err, asked)
	}
	os.Remove(filepath.Join(ws, "notes.txt"))
	res, err = h.svc().Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "claude-code", Profile: "controlled"}, RunOptions{})
	if err != nil || res.Output != "denied" {
		t.Fatalf("default must deny: %+v %v", res, err)
	}
}

func TestMultiTurnAndResume(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	ctx := context.Background()
	sess, err := svc.OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("unused"), Provider: "opencode"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"one", "two"} {
		r, err := sess.Prompt(ctx, domain.Text(p))
		if err != nil || !strings.Contains(r.Output, p) {
			t.Fatalf("%s: %+v %v", p, r, err)
		}
	}
	id := sess.Record().ID
	native := sess.Record().NativeID
	sess.Close(ctx)
	if rec, _ := svc.SessionRecord(ctx, id); rec.Turns != 2 {
		t.Fatalf("turns %d", rec.Turns)
	}
	again, err := svc.ResumeSession(ctx, id, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close(ctx)
	if again.Record().ID != id || again.Record().NativeID != native {
		t.Fatalf("resumed %+v", again.Record())
	}
	if _, err := again.Prompt(ctx, domain.Text("three")); err != nil {
		t.Fatal(err)
	}
	if again.Record().Turns != 3 {
		t.Fatalf("turns after resume %d", again.Record().Turns)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	h := newRunHarness(t)
	h.cfg.Execution.MaxConcurrentSessions = 1
	svc := h.svc()
	ctx := context.Background()
	first, err := svc.OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if _, err := svc.OpenSession(wctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{}); domain.CodeOf(err) != domain.ErrCancelled {
		t.Fatalf("second session should wait for a slot: %v", err)
	}
	first.Close(ctx)
	second, err := svc.OpenSession(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{})
	if err != nil {
		t.Fatalf("slot not released: %v", err)
	}
	second.Close(ctx)
}

func TestStaleSessionsAreInterrupted(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	ctx := context.Background()
	h.store.Put(ctx, sessionCollection, "ses_old", domain.SessionRecord{ID: "ses_old", State: domain.StateRunning, OwnerPID: 999999, CreatedAt: time.Now()})
	list, err := svc.Sessions(ctx)
	if err != nil || len(list) != 1 || list[0].State != domain.StateInterrupted {
		t.Fatalf("%+v %v", list, err)
	}
	if err := svc.CancelSession(ctx, "ses_old"); domain.CodeOf(err) != domain.ErrSessionNotFound {
		t.Fatalf("cancel of a dead session: %v", err)
	}
}

func TestFailedOpenIsRecorded(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "auth"
	svc := h.svc()
	_, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode"}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrAuthenticationRequired {
		t.Fatalf("got %v", err)
	}
	list, _ := svc.Sessions(context.Background())
	if len(list) != 1 || list[0].State != domain.StateFailed || list[0].LastError == "" {
		t.Fatalf("%+v", list)
	}
}

func TestTasksRunOnTheChosenClaudeAccount(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "env"
	// The caller's shell points at one account; claude-code must not inherit it.
	h.env["CLAUDE_CONFIG_DIR"] = filepath.Join(h.home, ".claude-alt")
	os.MkdirAll(filepath.Join(h.home, ".claude-alt"), 0o755)
	wrapper := filepath.Join(h.bin, "claude-work")
	os.WriteFile(wrapper, []byte("#!/bin/sh\nexport CLAUDE_CONFIG_DIR=\"$HOME/.claude-work\"\nexec claude \"$@\"\n"), 0o755)
	svc := h.svc()
	cases := map[string]string{
		"claude-code":      "CLAUDE_CONFIG_DIR=",
		"claude-code-alt":  "CLAUDE_CONFIG_DIR=" + filepath.Join(h.home, ".claude-alt"),
		"claude-code-work": "CLAUDE_CONFIG_DIR=" + filepath.Join(h.home, ".claude-work"),
	}
	for provider, want := range cases {
		res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: provider}, RunOptions{})
		if err != nil || res.Output != want {
			t.Errorf("%s: got %q %v, want %q", provider, res.Output, err, want)
		}
		if rec, _ := svc.SessionRecord(context.Background(), res.SessionID); rec.ProviderID != provider {
			t.Errorf("%s: session recorded as %s", provider, rec.ProviderID)
		}
	}
}

func TestErrorsNameTheAccount(t *testing.T) {
	h := newRunHarness(t)
	h.env[fakeclaude.EnvScenario] = "authexpired"
	os.MkdirAll(filepath.Join(h.home, ".claude-alt"), 0o755)
	_, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code-alt"}, RunOptions{})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.ErrAuthenticationRequired || de.Provider != "claude-code-alt" {
		t.Fatalf("got %#v", err)
	}
}
