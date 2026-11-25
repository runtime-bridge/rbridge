//go:build unix

package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
