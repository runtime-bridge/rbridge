//go:build unix

package bridge_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
	"github.com/runtime-bridge/rbridge/pkg/bridge"
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

// setup puts the test binary on PATH as claude and opencode and gives rbridge
// a config that lets the fake's control variables through.
func setup(t *testing.T) (bridge.Options, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	exe, _ := os.Executable()
	for _, n := range []string{"claude", "opencode"} {
		os.Symlink(exe, filepath.Join(bin, n))
	}
	rb := filepath.Join(home, "rb")
	os.MkdirAll(filepath.Join(rb, "config"), 0o755)
	os.WriteFile(filepath.Join(rb, "config", "config.yaml"), []byte(`version: 1
policy:
  env_allowlist: [PATH, HOME, RBRIDGE_FAKE_SVC, RBRIDGE_FAKE_ACP_SCENARIO, RBRIDGE_FAKE_CLAUDE_SCENARIO]
execution:
  kill_grace_period: 1s
`), 0o644)
	ws := filepath.Join(home, "ws")
	os.MkdirAll(ws, 0o755)
	return bridge.Options{Home: rb, Environ: []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "RBRIDGE_FAKE_SVC=1"}}, ws
}

func TestRunAndEvents(t *testing.T) {
	opts, ws := setup(t)
	ctx := context.Background()
	b, err := bridge.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	providers, err := b.Providers(ctx, true)
	if err != nil || len(providers) != 5 {
		t.Fatalf("providers %+v %v", providers, err)
	}
	var types []bridge.EventType
	res, err := b.Run(ctx, bridge.Task{Workspace: ws, Prompt: bridge.Text("from the library"), Provider: "opencode"},
		bridge.OnEvent(func(e bridge.Event) { types = append(types, e.Type) }))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "from the library") || res.StopReason != bridge.StopEndTurn {
		t.Fatalf("%+v", res)
	}
	if types[0] != bridge.EventNotice || !slices.Contains(types, bridge.EventSessionStarted) || types[len(types)-1] != bridge.EventSessionClosed {
		t.Fatalf("events %v", types)
	}
}

func TestSessionsAndSkills(t *testing.T) {
	opts, ws := setup(t)
	ctx := context.Background()
	b, err := bridge.New(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "doc-writer")
	os.MkdirAll(src, 0o755)
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: doc-writer\ndescription: Write docs.\n---\n"), 0o644)
	rec, err := b.AddSkill(ctx, src, false)
	if err != nil || rec.Trust != bridge.TrustUnreviewed {
		t.Fatalf("%+v %v", rec, err)
	}
	_, err = b.Run(ctx, bridge.Task{Workspace: ws, Prompt: bridge.Text("x"), Skills: []string{"doc-writer"}})
	if bridge.CodeOf(err) != bridge.ErrSkillBlocked {
		t.Fatalf("unreviewed skill should be refused: %v", err)
	}
	if _, err := b.SetSkillTrust(ctx, "doc-writer", bridge.TrustReviewed); err != nil {
		t.Fatal(err)
	}
	s, err := b.Open(ctx, bridge.Task{Workspace: ws, Skills: []string{"doc-writer"}, Provider: "claude-code"}, bridge.AllowAll())
	if err != nil {
		t.Fatal(err)
	}
	r1, err := s.Prompt(ctx, bridge.Text("first"))
	if err != nil || !strings.Contains(r1.Output, "first") || len(r1.Skills) != 1 {
		t.Fatalf("%+v %v", r1, err)
	}
	id := s.ID()
	s.Close(ctx)
	list, _ := b.Sessions(ctx)
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("sessions %+v", list)
	}
	s2, err := b.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close(ctx)
	if _, err := s2.Prompt(ctx, bridge.Text("second")); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveSkill(ctx, "doc-writer"); err != nil {
		t.Fatal(err)
	}
}
