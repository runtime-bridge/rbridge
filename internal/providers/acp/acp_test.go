//go:build unix

package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/contract"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeacp.EnvEnable) == "1" {
		fakeacp.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func opener(ctx context.Context, t *testing.T, scenario string, opts domain.SessionOptions, sink domain.EventSink) (domain.ProviderSession, error) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts.Executable = exe
	opts.Env = []string{fakeacp.EnvEnable + "=1", fakeacp.EnvScenario + "=" + scenario, "PATH=" + os.Getenv("PATH")}
	return Open(ctx, domain.CapabilitySet{domain.CapSkills: {Supported: domain.SupportYes, Source: domain.SourceManifest}}, Launch{}, opts, sink)
}

func TestContract(t *testing.T) { contract.Run(t, opener) }

func openScenario(t *testing.T, scenario string, mutate func(*domain.SessionOptions)) (*Session, *contract.Recorder, domain.SessionOptions) {
	t.Helper()
	ws := t.TempDir()
	ws, _ = filepath.EvalSymlinks(ws)
	opts := domain.SessionOptions{BridgeID: "b", Workspace: ws, KillGrace: time.Second}
	if mutate != nil {
		mutate(&opts)
	}
	rec := &contract.Recorder{}
	s, err := opener(context.Background(), t, scenario, opts, rec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s.(*Session), rec, opts
}

func TestHandshakeCapabilitiesBeatManifest(t *testing.T) {
	s, _, _ := openScenario(t, "echo", nil)
	c := s.Capabilities()
	if c[domain.CapSessionsLoad].Source != domain.SourceHandshake || c[domain.CapSessionsLoad].Supported != domain.SupportYes {
		t.Fatalf("sessions.load = %+v", c[domain.CapSessionsLoad])
	}
	if c[domain.CapPromptAudio].Supported != domain.SupportNo {
		t.Fatalf("audio should be no: %+v", c[domain.CapPromptAudio])
	}
	if c[domain.CapSkills].Source != domain.SourceManifest {
		t.Fatal("manifest values not reported by the runtime must be kept")
	}
}

func TestReadFileStaysInWorkspace(t *testing.T) {
	s, _, opts := openScenario(t, "readfile", nil)
	inside := filepath.Join(opts.Workspace, "in.txt")
	os.WriteFile(inside, []byte("secret-inside"), 0o644)
	res, err := s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text(inside)})
	if err != nil || !strings.Contains(res.Output, "secret-inside") {
		t.Fatalf("inside read: %+v %v", res, err)
	}

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "out.txt")
	os.WriteFile(outside, []byte("must-not-leak"), 0o644)
	res, _ = s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text(outside)})
	if strings.Contains(res.Output, "must-not-leak") || !strings.Contains(res.Output, "outside the workspace") {
		t.Fatalf("outside read allowed: %q", res.Output)
	}

	// A symlink inside the workspace pointing out of it is refused too.
	link := filepath.Join(opts.Workspace, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	res, _ = s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text(link)})
	if strings.Contains(res.Output, "must-not-leak") {
		t.Fatalf("symlink escape allowed: %q", res.Output)
	}
	res, _ = s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text(filepath.Join(opts.Workspace, "..", filepath.Base(outsideDir), "out.txt"))})
	if strings.Contains(res.Output, "must-not-leak") {
		t.Fatalf("dot-dot escape allowed: %q", res.Output)
	}
}

func TestResume(t *testing.T) {
	s, _, _ := openScenario(t, "echo", func(o *domain.SessionOptions) { o.ResumeNativeID = "previous-id" })
	if s.NativeID() != "previous-id" {
		t.Fatalf("native id %q", s.NativeID())
	}
}

func TestModelRejected(t *testing.T) {
	exe, _ := os.Executable()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	_, err := Open(context.Background(), nil, Launch{}, domain.SessionOptions{
		Workspace: ws, Executable: exe, Model: "bad-model", KillGrace: time.Second,
		Env: []string{fakeacp.EnvEnable + "=1", fakeacp.EnvScenario + "=echo"},
	}, nil)
	if domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("want invalid_input, got %v", err)
	}
}

func TestRelativeWorkspaceRejectedBeforeStart(t *testing.T) {
	_, err := Open(context.Background(), nil, Launch{}, domain.SessionOptions{Workspace: "rel", Executable: "/bin/true"}, nil)
	if domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("got %v", err)
	}
}

func TestMCPServerValidation(t *testing.T) {
	_, err := Open(context.Background(), nil, Launch{}, domain.SessionOptions{}, nil)
	if err == nil {
		t.Fatal("empty options accepted")
	}
	exe, _ := os.Executable()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	_, err = Open(context.Background(), nil, Launch{}, domain.SessionOptions{
		Workspace: ws, Executable: exe, KillGrace: time.Second,
		Env:        []string{fakeacp.EnvEnable + "=1", fakeacp.EnvScenario + "=echo"},
		MCPServers: []domain.MCPServer{{Name: "x", Command: "relative-cmd"}},
	}, nil)
	if domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("relative MCP command: %v", err)
	}
}

// BenchmarkOpenAndTurn measures starting an ACP agent, the handshake and one
// turn, against the fake agent. Real runtimes add their own start-up time.
func BenchmarkOpenAndTurn(b *testing.B) {
	exe, _ := os.Executable()
	ws, _ := filepath.EvalSymlinks(b.TempDir())
	opts := domain.SessionOptions{Workspace: ws, Executable: exe, KillGrace: time.Second, Env: []string{fakeacp.EnvEnable + "=1", fakeacp.EnvScenario + "=echo"}}
	for b.Loop() {
		s, err := Open(context.Background(), nil, Launch{}, opts, nil)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("x")}); err != nil {
			b.Fatal(err)
		}
		s.Close(context.Background())
	}
}

func terminalSession(t *testing.T, mutate func(*domain.SessionOptions)) *Session {
	t.Helper()
	s, _, _ := openScenario(t, "terminal", func(o *domain.SessionOptions) {
		o.ClientTerminal = true
		o.CommandTimeout = 5 * time.Second
		o.MaxOutputBytes = 1 << 16
		if mutate != nil {
			mutate(o)
		}
	})
	return s
}

func turn(t *testing.T, s *Session, text string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := s.Prompt(ctx, domain.Turn{Prompt: domain.Text(text)})
	if err != nil {
		t.Fatal(err)
	}
	return res.Output
}

func TestTerminals(t *testing.T) {
	var gated []string
	s := terminalSession(t, func(o *domain.SessionOptions) {
		o.CommandGate = func(_ context.Context, cmd string, args []string) error {
			gated = append(gated, cmd+" "+strings.Join(args, " "))
			if strings.Contains(strings.Join(args, " "), "forbidden") {
				return errors.New("denied by policy")
			}
			return nil
		}
	})
	if out := turn(t, s, "echo hello from terminal; pwd"); !strings.HasPrefix(out, "exit=0 hello from terminal") || !strings.Contains(out, s.opts.Workspace) {
		t.Fatalf("output %q", out)
	}
	if out := turn(t, s, "exit 7"); !strings.HasPrefix(out, "exit=7") {
		t.Fatalf("exit code: %q", out)
	}
	if out := turn(t, s, "echo forbidden"); !strings.Contains(out, "denied by policy") {
		t.Fatalf("gate: %q", out)
	}
	if len(gated) != 3 {
		t.Fatalf("gate calls %v", gated)
	}
	// The command sees the session's filtered environment, not rbridge's.
	t.Setenv("RBRIDGE_TERMINAL_LEAK", "leaked")
	if out := turn(t, s, `echo "[$RBRIDGE_TERMINAL_LEAK]"`); !strings.Contains(out, "[]") {
		t.Fatalf("environment leaked: %q", out)
	}
}

func TestTerminalLimits(t *testing.T) {
	s := terminalSession(t, func(o *domain.SessionOptions) {
		o.CommandTimeout = 300 * time.Millisecond
		o.MaxOutputBytes = 100
	})
	start := time.Now()
	if out := turn(t, s, "sleep 30"); !strings.HasPrefix(out, "signal=") || time.Since(start) > 10*time.Second {
		t.Fatalf("timeout: %q after %s", out, time.Since(start))
	}
	out := turn(t, s, `i=0; while [ $i -lt 500 ]; do echo line-$i; i=$((i+1)); done`)
	if !strings.Contains(out, "truncated") || !strings.Contains(out, "line-499") || strings.Contains(out, "line-1\n") {
		t.Fatalf("output limit keeps the end: %q", out)
	}
}

func TestTerminalNotOffered(t *testing.T) {
	s, _, _ := openScenario(t, "terminal", nil)
	if out := turn(t, s, "echo hi"); !strings.Contains(out, "does not offer terminals") {
		t.Fatalf("got %q", out)
	}
}

func TestTerminalCleanupOnClose(t *testing.T) {
	s := terminalSession(t, func(o *domain.SessionOptions) { o.CommandTimeout = time.Minute })
	ws := s.opts.Workspace
	go s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("sleep 60 & echo $! > " + filepath.Join(ws, "bg.pid") + "; wait")})
	var pid int
	deadline := time.Now().Add(10 * time.Second)
	for pid == 0 && time.Now().Before(deadline) {
		b, _ := os.ReadFile(filepath.Join(ws, "bg.pid"))
		fmt.Sscan(strings.TrimSpace(string(b)), &pid)
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("background command did not start")
	}
	s.Close(context.Background())
	deadline = time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("terminal child %d survived Close", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestModesAndOptions(t *testing.T) {
	s, rec, _ := openScenario(t, "echo", nil)
	cur, modes := s.Modes()
	if cur != "ask" || len(modes) != 2 || modes[1].Description != "Edits files" {
		t.Fatalf("modes %s %+v", cur, modes)
	}
	if err := s.SetMode(context.Background(), "code"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMode(context.Background(), "nope"); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("unknown mode: %v", err)
	}
	opts := s.ConfigOptions()
	if len(opts) != 1 || opts[0].ID != "model" || opts[0].Current != "small" || len(opts[0].Options) != 2 {
		t.Fatalf("options %+v", opts)
	}
	if err := s.SetConfigOption(context.Background(), "model", "large"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetConfigOption(context.Background(), "model", "huge"); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("bad value: %v", err)
	}
	if got := s.ConfigOptions()[0].Current; got != "large" {
		t.Fatalf("current after set: %s", got)
	}
	if out := turn(t, s, "mode?"); out != "mode: code model: large" {
		t.Fatalf("agent state %q", out)
	}
	if !rec.Has(domain.EventModeUpdated) {
		t.Fatal("mode change not reported")
	}
}

func TestElicitation(t *testing.T) {
	answer := func(r domain.InputResponse) domain.Elicitor {
		return domain.ElicitorFunc(func(_ context.Context, req domain.InputRequest) domain.InputResponse {
			if req.Mode != "form" || !strings.Contains(string(req.Schema), `"name"`) || req.Message != "What is your name?" {
				t.Errorf("request %+v", req)
			}
			return r
		})
	}
	s, rec, _ := openScenario(t, "elicit", func(o *domain.SessionOptions) {
		o.Elicitor = answer(domain.InputResponse{Action: "accept", Content: map[string]any{"name": "Ada"}})
	})
	if out := turn(t, s, "x"); out != "hello Ada" {
		t.Fatalf("accept: %q", out)
	}
	if !rec.Has(domain.EventInputRequired) || !rec.Has(domain.EventInputResolved) {
		t.Fatal("input events missing")
	}
	s2, _, _ := openScenario(t, "elicit", func(o *domain.SessionOptions) {
		o.Elicitor = answer(domain.InputResponse{Action: "decline"})
	})
	if out := turn(t, s2, "x"); out != "declined" {
		t.Fatalf("decline: %q", out)
	}
	s3, _, _ := openScenario(t, "elicit", nil)
	if out := turn(t, s3, "x"); out != "declined" && !strings.Contains(out, "elicitation error") {
		t.Fatalf("no elicitor: %q", out)
	}
}

func TestProtocolVersionMismatch(t *testing.T) {
	exe, _ := os.Executable()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	_, err := Open(context.Background(), nil, Launch{}, domain.SessionOptions{
		Workspace: ws, Executable: exe, KillGrace: time.Second,
		Env: []string{fakeacp.EnvEnable + "=1", fakeacp.EnvScenario + "=echo", "RBRIDGE_FAKE_ACP_PROTOCOL=2"},
	}, nil)
	if domain.CodeOf(err) != domain.ErrProviderProtocol || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("got %v", err)
	}
}
