//go:build linux

package service

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
	"github.com/runtime-bridge/rbridge/internal/sandbox"
)

func TestSandboxedSession(t *testing.T) {
	if e, notes := sandbox.Detect(context.Background(), "auto"); e == nil {
		t.Skipf("no sandbox engine here: %v", notes)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	outside, err := os.MkdirTemp(".", "outside-")
	if err != nil {
		t.Fatal(err)
	}
	outside, _ = filepath.Abs(outside)
	defer os.RemoveAll(outside)

	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "escape"
	svc := h.svc()
	ctx := context.Background()
	run := func(mode string) (string, domain.SessionRecord, []string) {
		t.Helper()
		ws := h.workspace()
		os.Remove(filepath.Join(outside, "out.txt"))
		prompt := "write:" + filepath.Join(ws, "in.txt") + "," + filepath.Join(outside, "out.txt") + " dial:" + ln.Addr().String()
		var notices []string
		res, err := svc.Run(ctx, domain.Task{Workspace: ws, Prompt: domain.Text(prompt), Provider: "opencode", Sandbox: mode},
			RunOptions{Sink: domain.EventSinkFunc(func(e domain.Event) {
				if e.Type == domain.EventNotice {
					notices = append(notices, string(e.Data))
				}
			})})
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		rec, _ := svc.SessionRecord(ctx, res.SessionID)
		return res.Output, rec, notices
	}

	if out, _, _ := run("off"); out != "write in.txt: ok; write out.txt: ok; dial: ok" {
		t.Fatalf("without a sandbox the agent can do anything: %q", out)
	}
	out, rec, _ := run("files")
	if out != "write in.txt: ok; write out.txt: denied; dial: ok" || !strings.HasSuffix(rec.Sandbox, ":files") {
		t.Fatalf("files: %q %+v", out, rec.Sandbox)
	}
	out, rec, notices := run("full")
	if out != "write in.txt: ok; write out.txt: denied; dial: denied" || !strings.HasSuffix(rec.Sandbox, ":full") {
		t.Fatalf("full: %q %s", out, rec.Sandbox)
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); err == nil {
		t.Fatal("the sandboxed agent wrote outside the workspace")
	}
	joined := strings.Join(notices, " ")
	if !strings.Contains(joined, "enforced by rbridge: environment, process lifetime, skill scripts, file writes, client terminals, network") {
		t.Fatalf("enforcement notice: %s", joined)
	}
	recs, _ := svc.Audit().Read(time.Time{})
	if len(recs) == 0 || !strings.HasSuffix(recs[len(recs)-1].Sandbox, ":full") {
		t.Fatalf("audit %+v", recs)
	}
}

func TestSandboxRequiredButMissing(t *testing.T) {
	h := newRunHarness(t)
	h.cfg.Sandbox.Engine = "bwrap"
	svc := h.svc()
	if e, _ := svc.engine(context.Background()); e != nil {
		t.Skip("bubblewrap works here")
	}
	_, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "opencode", Sandbox: "full"}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrPolicyUnenforceable || !strings.Contains(err.Error(), "no sandbox engine works") {
		t.Fatalf("a missing sandbox must fail the task: %v", err)
	}
}

func TestSandboxedTerminal(t *testing.T) {
	if e, notes := sandbox.Detect(context.Background(), "auto"); e == nil {
		t.Skipf("no sandbox engine here: %v", notes)
	}
	outside, err := os.MkdirTemp(".", "outside-")
	if err != nil {
		t.Fatal(err)
	}
	outside, _ = filepath.Abs(outside)
	defer os.RemoveAll(outside)
	h := newRunHarness(t)
	h.env[fakeacp.EnvScenario] = "terminal"
	h.cfg.Policy.Custom.Rules = map[string]string{"execute": "allow"}
	svc := h.svc()
	ws := h.workspace()
	cmd := "touch " + filepath.Join(ws, "in.txt") + " && echo inside-ok; touch " + filepath.Join(outside, "out.txt") + " 2>/dev/null || echo outside-denied"
	res, err := svc.Run(context.Background(), domain.Task{Workspace: ws, Prompt: domain.Text(cmd), Provider: "opencode", Profile: "custom", Sandbox: "full"}, RunOptions{})
	if err != nil || !strings.Contains(res.Output, "inside-ok") || !strings.Contains(res.Output, "outside-denied") {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "out.txt")); err == nil {
		t.Fatal("a terminal command escaped the sandbox")
	}
}
