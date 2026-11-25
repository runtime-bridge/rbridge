//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
	"github.com/runtime-bridge/rbridge/pkg/bridge"
)

// The smoke test runs the example against a fake Claude Code: the test
// binary itself, on PATH as "claude".
func TestMain(m *testing.M) {
	if os.Getenv("RBRIDGE_FAKE_SVC") == "1" && filepath.Base(os.Args[0]) == "claude" {
		fakeclaude.Main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestReleaseNotes(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	exe, _ := os.Executable()
	os.Symlink(exe, filepath.Join(bin, "claude"))
	rb := filepath.Join(home, "rb")
	os.MkdirAll(filepath.Join(rb, "config"), 0o755)
	os.WriteFile(filepath.Join(rb, "config", "config.yaml"), []byte("version: 1\npolicy:\n  env_allowlist: [PATH, HOME, RBRIDGE_FAKE_SVC]\n"), 0o644)

	repo := filepath.Join(home, "repo")
	os.MkdirAll(repo, 0o755)
	git := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	git("init", "-q")
	git("commit", "-q", "--allow-empty", "-m", "Initial release")
	git("tag", "v1.0.0")
	git("commit", "-q", "--allow-empty", "-m", "Add a --dry-run flag")
	git("commit", "-q", "--allow-empty", "-m", "Fix a crash on empty input")

	err := run(context.Background(), config{
		Repo: repo, Version: "v1.1.0", Out: "NOTES.md", Provider: "claude-code",
		Bridge: bridge.Options{Home: rb, Environ: []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "RBRIDGE_FAKE_SVC=1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(repo, "NOTES.md"))
	notes := string(b)
	// The fake echoes the prompt, so the notes show what the runtime was given.
	for _, want := range []string{"## v1.1.0", "v1.0.0 to HEAD", "Add a --dry-run flag", "Fix a crash on empty input"} {
		if !strings.Contains(notes, want) {
			t.Fatalf("missing %q in:\n%s", want, notes)
		}
	}
	if strings.Contains(notes, "Initial release") {
		t.Fatal("commits before the tag were included")
	}
}
