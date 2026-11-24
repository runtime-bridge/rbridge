//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbridge/rbridge/internal/providers/fakeacp"
	"github.com/rbridge/rbridge/internal/providers/fakeclaude"
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

type fakeCLI struct {
	t    *testing.T
	home string
	ws   string
	vars map[string]string
}

func newFakeCLI(t *testing.T) *fakeCLI {
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
	os.WriteFile(filepath.Join(rb, "config", "config.yaml"), []byte("version: 1\npolicy:\n  env_allowlist: [PATH, HOME, RBRIDGE_FAKE_SVC, RBRIDGE_FAKE_ACP_SCENARIO, RBRIDGE_FAKE_CLAUDE_SCENARIO]\nexecution:\n  kill_grace_period: 1s\n"), 0o644)
	ws := filepath.Join(home, "ws")
	os.MkdirAll(ws, 0o755)
	return &fakeCLI{t: t, home: home, ws: ws, vars: map[string]string{
		"PATH": bin + ":/usr/bin:/bin", "HOME": home, "RBRIDGE_HOME": rb, "RBRIDGE_FAKE_SVC": "1",
	}}
}

func (f *fakeCLI) run(stdin string, args ...string) run {
	var out, errb bytes.Buffer
	code := Run(context.Background(), args, Env{
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Getenv: func(k string) string { return f.vars[k] },
		Environ: func() []string {
			var e []string
			for k, v := range f.vars {
				e = append(e, k+"="+v)
			}
			return e
		},
		Home: f.home, Dir: f.ws, GOOS: "linux", UID: os.Getuid(),
	})
	return run{code, out.String(), errb.String()}
}

func TestRunCommand(t *testing.T) {
	f := newFakeCLI(t)
	r := f.run("", "run", "--provider", "opencode", "hello", "world")
	if r.code != 0 || !strings.Contains(r.stdout, "echo: hello world") || !strings.Contains(r.stderr, "[done]") {
		t.Fatalf("code %d\nstdout %s\nstderr %s", r.code, r.stdout, r.stderr)
	}
	r = f.run("", "run", "--quiet", "--provider", "claude-code", "quiet please")
	if r.code != 0 || strings.TrimSpace(r.stdout) != "echo: quiet please" || r.stderr != "" {
		t.Fatalf("quiet: %+v", r)
	}
	r = f.run("piped prompt\n", "run", "--quiet", "--provider", "opencode")
	if r.code != 0 || !strings.Contains(r.stdout, "piped prompt") {
		t.Fatalf("stdin prompt: %+v", r)
	}
}

func TestRunJSON(t *testing.T) {
	f := newFakeCLI(t)
	r := f.run("", "run", "--json", "--provider", "opencode", "json out")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	var last struct {
		Type   string `json:"type"`
		Result struct {
			Output string `json:"output"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Type != "result" || !strings.Contains(last.Result.Output, "json out") {
		t.Fatalf("last line %q: %v", lines[len(lines)-1], err)
	}
	for _, l := range lines[:len(lines)-1] {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil || e["seq"] == nil {
			t.Fatalf("event line %q", l)
		}
	}
}

func TestRunApprovals(t *testing.T) {
	f := newFakeCLI(t)
	f.vars["RBRIDGE_FAKE_CLAUDE_SCENARIO"] = "permission"
	r := f.run("", "run", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "denied" {
		t.Fatalf("non-interactive default must deny: %+v", r)
	}
	r = f.run("", "run", "--approve", "allow", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "allowed" {
		t.Fatalf("allow: %+v", r)
	}
	os.Remove(filepath.Join(f.ws, "notes.txt"))
	r = f.run("y\n", "run", "--approve", "ask", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "allowed" || !strings.Contains(r.stderr, "allow?") {
		t.Fatalf("ask yes: %+v", r)
	}
	r = f.run("n\n", "run", "--approve", "ask", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "denied" {
		t.Fatalf("ask no: %+v", r)
	}
	if r := f.run("", "run", "--approve", "maybe", "x"); r.code != 2 {
		t.Fatalf("bad --approve: %+v", r)
	}
}

func TestChatAndSessions(t *testing.T) {
	f := newFakeCLI(t)
	r := f.run("second line\n/exit\n", "chat", "--quiet", "--provider", "opencode", "first line")
	if r.code != 0 || !strings.Contains(r.stdout, "echo: first line") || !strings.Contains(r.stdout, "echo: second line") {
		t.Fatalf("chat: %+v", r)
	}
	r = f.run("", "sessions", "--json")
	var list []struct {
		ID    string `json:"id"`
		Turns int    `json:"turns"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil || len(list) != 1 || list[0].Turns != 2 || list[0].State != "closed" {
		t.Fatalf("sessions: %s %v", r.stdout, err)
	}
	id := list[0].ID
	if r := f.run("", "sessions", "show", id); r.code != 0 || !strings.Contains(r.stdout, "turns:      2") {
		t.Fatalf("show: %+v", r)
	}
	if r := f.run("", "sessions", "resume", "--quiet", id, "third"); r.code != 0 || !strings.Contains(r.stdout, "echo: third") {
		t.Fatalf("resume: %+v", r)
	}
	if r := f.run("", "sessions", "cancel", id); r.code != 50 {
		t.Fatalf("cancel of a closed session: %+v", r)
	}
	if r := f.run("", "sessions", "show", "ses_nope"); r.code != 50 {
		t.Fatalf("missing session: %+v", r)
	}
}

func writeSkill(t *testing.T, name string) string {
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: A test skill.\nlicense: MIT\n---\n# Steps\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "scripts", "go.sh"), []byte("echo\n"), 0o755)
	return dir
}

func TestSkillsCommands(t *testing.T) {
	f := newFakeCLI(t)
	src := writeSkill(t, "code-review")
	r := f.run("", "skills", "add", src)
	if r.code != 0 || !strings.Contains(r.stdout, "added code-review") || !strings.Contains(r.stdout, "trust unreviewed") {
		t.Fatalf("add: %+v", r)
	}
	if r := f.run("", "skills", "add", src); !strings.Contains(r.stdout, "already installed") {
		t.Fatalf("re-add: %+v", r)
	}
	if r := f.run("", "skills"); !strings.Contains(r.stdout, "code-review") || !strings.Contains(r.stdout, "unreviewed") {
		t.Fatalf("list: %+v", r)
	}
	if r := f.run("", "skills", "show", "code-review"); !strings.Contains(r.stdout, "scripts/go.sh  (script)") || !strings.Contains(r.stdout, "verify:    ok") {
		t.Fatalf("show: %+v", r)
	}
	if r := f.run("", "run", "--skill", "code-review", "x"); r.code != 22 {
		t.Fatalf("unreviewed skill must be refused with exit 22: %+v", r)
	}
	if r := f.run("", "skills", "trust", "code-review", "reviewed"); r.code != 0 {
		t.Fatalf("trust: %+v", r)
	}
	r = f.run("", "run", "--provider", "claude-code", "--skill", "code-review", "x")
	if r.code != 0 || !strings.Contains(r.stderr, "[skill] code-review delivered") {
		t.Fatalf("run with skill: %+v", r)
	}
	if r := f.run("", "skills", "verify"); r.code != 0 || !strings.Contains(r.stdout, "ok   code-review") {
		t.Fatalf("verify: %+v", r)
	}
	out := filepath.Join(t.TempDir(), "layout")
	r = f.run("", "skills", "deliver", "code-review", "--provider", "claude-code", "--to", out)
	if r.code != 0 || !strings.Contains(r.stdout, "--add-dir") {
		t.Fatalf("deliver: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(out, "skills-session", ".claude", "skills", "code-review", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if r := f.run("", "skills", "remove", "code-review"); r.code != 0 {
		t.Fatalf("remove: %+v", r)
	}
	if r := f.run("", "skills", "show", "code-review"); r.code != 20 {
		t.Fatalf("show after remove: %+v", r)
	}
	if r := f.run("", "skills", "add", filepath.Join(t.TempDir(), "missing")); r.code == 0 {
		t.Fatal("adding a missing dir should fail")
	}
}

func TestAddWithTrust(t *testing.T) {
	f := newFakeCLI(t)
	if r := f.run("", "skills", "add", "--trust", "trusted", writeSkill(t, "doc-writer")); r.code != 0 || strings.Contains(r.stdout, "review it with") {
		t.Fatalf("%+v", r)
	}
	if r := f.run("", "skills", "list", "--json"); !strings.Contains(r.stdout, `"trust": "trusted"`) {
		t.Fatalf("%+v", r)
	}
}
