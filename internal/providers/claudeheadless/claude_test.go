//go:build unix

package claudeheadless

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/providers/contract"
	"github.com/rbridge/rbridge/internal/providers/fakeclaude"
)

func TestMain(m *testing.M) {
	if os.Getenv(fakeclaude.EnvEnable) == "1" {
		fakeclaude.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var launch = []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"}

func opener(ctx context.Context, t *testing.T, scenario string, opts domain.SessionOptions, sink domain.EventSink) (domain.ProviderSession, error) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts.Executable = exe
	opts.Env = append(opts.Env, fakeclaude.EnvEnable+"=1", fakeclaude.EnvScenario+"="+scenario, "PATH="+os.Getenv("PATH"))
	base := domain.CapabilitySet{domain.CapCancel: {Supported: domain.SupportYes, Source: domain.SourceManifest}}
	return Open(ctx, base, launch, opts, sink)
}

func TestContract(t *testing.T) { contract.Run(t, opener) }

func TestArgs(t *testing.T) {
	args := Args(launch, domain.SessionOptions{Model: "haiku", PermissionMode: "manual", SkillDirs: []string{"/a", "/b"}, OutputSchema: json.RawMessage(`{"type":"object"}`)}, "sid", "/tmp/mcp.json")
	joined := strings.Join(args, " ")
	for _, want := range []string{"--permission-prompts host --permission-prompt-tool stdio", "--permission-mode manual", "--session-id sid", "--model haiku", "--json-schema", "--mcp-config /tmp/mcp.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %s", want, joined)
		}
	}
	if !slices.Equal(args[len(args)-3:], []string{"--add-dir", "/a", "/b"}) {
		t.Errorf("--add-dir must come last: %v", args)
	}
	resumed := strings.Join(Args(launch, domain.SessionOptions{ResumeNativeID: "old"}, "new", ""), " ")
	if !strings.Contains(resumed, "--resume old") || strings.Contains(resumed, "--session-id") {
		t.Errorf("resume args: %s", resumed)
	}
}

func TestSessionStartedCarriesSkillsAndAuth(t *testing.T) {
	skills := t.TempDir()
	os.MkdirAll(filepath.Join(skills, ".claude", "skills", "security-audit"), 0o755)
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	rec := &contract.Recorder{}
	s, err := opener(context.Background(), t, "echo", domain.SessionOptions{Workspace: ws, SkillDirs: []string{skills}, KillGrace: time.Second}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("hi")}); err != nil {
		t.Fatal(err)
	}
	var started domain.SessionStartedData
	found := false
	for _, e := range rec.Events() {
		if e.Type == domain.EventSessionStarted {
			found = true
			json.Unmarshal(e.Data, &started)
		}
	}
	if !found {
		t.Fatal("no session.started")
	}
	if !slices.Contains(started.Skills, "security-audit") {
		t.Fatalf("skills = %v", started.Skills)
	}
	if s.NativeID() == "" {
		t.Fatal("no native id")
	}
}

func TestStructuredOutput(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	s, err := opener(context.Background(), t, "schema", domain.SessionOptions{Workspace: ws, OutputSchema: json.RawMessage(`{"type":"object"}`), KillGrace: time.Second}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	res, err := s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("json please")})
	if err != nil || string(res.Structured) != `{"ok":true}` {
		t.Fatalf("structured = %s err %v", res.Structured, err)
	}
}

func TestMCPConfigWritten(t *testing.T) {
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	scratch := t.TempDir()
	argsFile := filepath.Join(scratch, "args")
	s, err := opener(context.Background(), t, "echo", domain.SessionOptions{
		Workspace: ws, ScratchDir: scratch, KillGrace: time.Second,
		Env:        []string{fakeclaude.EnvArgsFile + "=" + argsFile},
		MCPServers: []domain.MCPServer{{Name: "files", Command: "/usr/bin/true"}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	b, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(b), filepath.Join(scratch, "mcp.json")) {
		t.Fatalf("args: %s", b)
	}
	cfg, _ := os.ReadFile(filepath.Join(scratch, "mcp.json"))
	if !strings.Contains(string(cfg), `"files"`) {
		t.Fatalf("mcp config: %s", cfg)
	}
	fi, _ := os.Stat(filepath.Join(scratch, "mcp.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mcp config mode %v", fi.Mode().Perm())
	}
}

func TestToolKinds(t *testing.T) {
	cases := map[string]string{"Read": "read", "Write": "edit", "Edit": "edit", "Bash": "execute", "Grep": "search", "WebFetch": "fetch", "Mystery": "other"}
	for in, want := range cases {
		if got := toolKind(in); got != want {
			t.Errorf("%s -> %s, want %s", in, got, want)
		}
	}
	if got := approvalTitle("Bash", json.RawMessage(`{"command":"rm -rf x"}`)); got != "Bash: rm -rf x" {
		t.Errorf("title %q", got)
	}
}
