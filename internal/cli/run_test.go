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

	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
	"github.com/runtime-bridge/rbridge/internal/skills/lockfile"
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
	r := f.run("", "run", "--profile", "controlled", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "denied" {
		t.Fatalf("non-interactive default must deny: %+v", r)
	}
	r = f.run("", "run", "--profile", "controlled", "--approve", "allow", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "allowed" {
		t.Fatalf("allow: %+v", r)
	}
	os.Remove(filepath.Join(f.ws, "notes.txt"))
	r = f.run("y\n", "run", "--profile", "controlled", "--approve", "ask", "--quiet", "--provider", "claude-code", "write")
	if strings.TrimSpace(r.stdout) != "allowed" || !strings.Contains(r.stderr, "allow?") {
		t.Fatalf("ask yes: %+v", r)
	}
	r = f.run("n\n", "run", "--profile", "controlled", "--approve", "ask", "--quiet", "--provider", "claude-code", "write")
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

func TestAuditCommand(t *testing.T) {
	f := newFakeCLI(t)
	f.vars["RBRIDGE_FAKE_CLAUDE_SCENARIO"] = "permission"
	if r := f.run("", "run", "--profile", "observe", "--quiet", "--provider", "claude-code", "x"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	r := f.run("", "audit")
	if r.code != 0 || !strings.Contains(r.stdout, "observe") || !strings.Contains(r.stdout, "0 allowed, 1 denied") || !strings.Contains(r.stdout, "network") {
		t.Fatalf("%+v", r)
	}
	if r := f.run("", "audit", "--since", "2000-01-01", "--json"); !strings.Contains(r.stdout, `"profile": "observe"`) {
		t.Fatalf("%+v", r)
	}
	if r := f.run("", "audit", "--since", "yesterday"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestMCPServer(t *testing.T) {
	f := newFakeCLI(t)
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rbridge_providers","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rbridge_run","arguments":{"workspace":"` + f.ws + `","prompt":"via mcp","provider":"opencode"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"rbridge_run","arguments":{"workspace":"relative","prompt":"x"}}}`,
		`{"jsonrpc":"2.0","id":6,"method":"nope"}`,
	}, "\n") + "\n"
	r := f.run(in, "mcp")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	byID := map[float64]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(r.stdout), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad line %q", line)
		}
		byID[m["id"].(float64)] = m
	}
	if v := byID[1]["result"].(map[string]any)["protocolVersion"]; v != "2025-06-18" {
		t.Errorf("version %v", v)
	}
	if n := len(byID[2]["result"].(map[string]any)["tools"].([]any)); n != 3 {
		t.Errorf("tools %d", n)
	}
	text := func(id float64) (string, bool) {
		res := byID[id]["result"].(map[string]any)
		return res["content"].([]any)[0].(map[string]any)["text"].(string), res["isError"].(bool)
	}
	if s, isErr := text(3); isErr || !strings.Contains(s, "opencode: available") {
		t.Errorf("providers: %q", s)
	}
	if s, isErr := text(4); isErr || s != "echo: via mcp" {
		t.Errorf("run: %q %v", s, isErr)
	}
	if _, isErr := text(5); !isErr {
		t.Error("relative workspace should be a tool error")
	}
	if byID[6]["error"] == nil {
		t.Error("unknown method should be a protocol error")
	}
}

func TestConfigCommand(t *testing.T) {
	f := newFakeCLI(t)
	r := f.run("", "config")
	if r.code != 0 || !strings.Contains(r.stdout, "#   user     loaded") || !strings.Contains(r.stdout, "env_allowlist:") {
		t.Fatalf("%+v", r)
	}
	r = f.run("", "config", "--defaults")
	if r.code != 0 || strings.Contains(r.stdout, "# layers") || !strings.Contains(r.stdout, "default_profile: standard") {
		t.Fatalf("%+v", r)
	}
}

func TestChatModesAndOptions(t *testing.T) {
	f := newFakeCLI(t)
	r := f.run("/modes\n/set model large\n/options\n/mode nope\nmode?\n/exit\n", "chat", "--quiet", "--provider", "opencode", "--mode", "code")
	if r.code != 0 || !strings.Contains(r.stdout, "mode: code model: large") {
		t.Fatalf("chat: %+v", r)
	}
	for _, want := range []string{"* code", "[set] model = large", "large      Model (small, large)", "offers no mode \"nope\""} {
		if !strings.Contains(r.stderr, want) {
			t.Fatalf("missing %q in %s", want, r.stderr)
		}
	}
}

func TestSkillsPermanentDelivery(t *testing.T) {
	f := newFakeCLI(t)
	if r := f.run("", "skills", "add", writeSkill(t, "fmt-rules")); r.code != 0 {
		t.Fatalf("add: %+v", r)
	}
	r := f.run("", "skills", "deliver", "fmt-rules", "--provider", "claude-code", "--permanent")
	dest := filepath.Join(f.home, ".claude", "skills", "fmt-rules")
	if r.code != 0 || !strings.Contains(r.stdout, dest+" (copy-no-scripts)") {
		t.Fatalf("deliver: %+v", r)
	}
	if r := f.run("", "skills", "deliveries"); !strings.Contains(r.stdout, "fmt-rules  claude-code  user") {
		t.Fatalf("deliveries: %+v", r)
	}
	if r := f.run("", "skills", "undeliver", "fmt-rules"); r.code != 0 || !strings.Contains(r.stdout, "removed "+dest) {
		t.Fatalf("undeliver: %+v", r)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("still delivered")
	}
}

func TestSkillsImportLock(t *testing.T) {
	f := newFakeCLI(t)
	dir := filepath.Join(f.ws, ".agents", "skills", "from-lock")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: from-lock\ndescription: d\n---\n"), 0o644)
	hash, _ := lockfile.Hash(dir)
	os.WriteFile(filepath.Join(f.ws, "skills-lock.json"), []byte(`{"version":2,"skills":{"from-lock":{"source":"o/r","sourceType":"github","computedHash":"`+hash+`"}}}`), 0o644)
	r := f.run("", "skills", "import", "--no-fetch")
	if r.code != 0 || !strings.Contains(r.stdout, "imported   from-lock from "+dir) {
		t.Fatalf("import: %+v", r)
	}
	if r := f.run("", "skills", "list"); !strings.Contains(r.stdout, "(from skills-lock.json)") {
		t.Fatalf("list: %+v", r)
	}
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: from-lock\ndescription: edited\n---\n"), 0o644)
	if r := f.run("", "skills", "import", "--no-fetch"); r.code == 0 || !strings.Contains(r.stdout, "refused") {
		t.Fatalf("edited copy: %+v", r)
	}
}

func TestSkillsShowCompatibility(t *testing.T) {
	f := newFakeCLI(t)
	f.run("", "skills", "add", "--trust", "reviewed", writeSkill(t, "cmp-skill"))
	if r := f.run("", "skills", "show", "cmp-skill"); !strings.Contains(r.stdout, "not used in a session yet") {
		t.Fatalf("before: %+v", r)
	}
	if r := f.run("", "run", "--provider", "claude-code", "--skill", "cmp-skill", "--quiet", "use-skill cmp-skill"); r.code != 0 {
		t.Fatalf("run: %+v", r)
	}
	r := f.run("", "skills", "show", "cmp-skill")
	if !strings.Contains(r.stdout, "claude-code") || !strings.Contains(r.stdout, "works") {
		t.Fatalf("after: %s", r.stdout)
	}
}

func TestSkillsAddManyAndPatterns(t *testing.T) {
	f := newFakeCLI(t)
	root := t.TempDir()
	for _, name := range []string{"evidence-rules", "metric-discovery", "metric-verification"} {
		src := writeSkill(t, name)
		if err := os.Rename(src, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	if r := f.run("", "skills", "add", root, "--exclude", "metric-*", "--dry-run"); r.code != 0 || !strings.Contains(r.stdout, "would add evidence-rules") || !strings.Contains(r.stdout, "skipped metric-discovery") {
		t.Fatalf("dry run: %+v", r)
	}
	if r := f.run("", "skills", "list"); !strings.Contains(r.stdout, "no skills yet") {
		t.Fatalf("dry run installed something: %s", r.stdout)
	}
	if r := f.run("", "skills", "add", root, "--only", "nosuch"); r.code == 0 {
		t.Fatal("an --only that names nothing should fail")
	}
	r := f.run("", "skills", "add", root)
	if r.code != 0 || !strings.Contains(r.stdout, "3 skills found: 3 added") || !strings.Contains(r.stdout, "skills trust evidence-rules metric-discovery metric-verification reviewed") {
		t.Fatalf("add all: %+v", r)
	}
	if r := f.run("", "skills", "trust", "metric-*", "evidence-rules", "reviewed"); r.code != 0 || strings.Count(r.stdout, "is now reviewed") != 3 {
		t.Fatalf("trust: %+v", r)
	}
	if r := f.run("", "skills", "trust", "nosuch", "reviewed"); r.code == 0 {
		t.Fatal("trusting an unknown skill should fail")
	}
	if r := f.run("", "skills", "remove", "metric-*"); r.code != 0 || strings.Count(r.stdout, "removed") != 2 {
		t.Fatalf("remove: %+v", r)
	}
}

func TestInit(t *testing.T) {
	f := newFakeCLI(t)
	home := filepath.Join(f.home, "fresh")
	f.vars["RBRIDGE_HOME"] = home
	cfgFile := filepath.Join(home, "config", "config.yaml")

	if r := f.run("", "init"); r.code != 0 || !strings.Contains(r.stdout, "wrote a starter file") {
		t.Fatalf("starter: %+v", r)
	}
	for _, d := range []string{"config", "data", "cache", "run"} {
		if _, err := os.Stat(filepath.Join(home, d)); err != nil {
			t.Fatalf("%s not created: %v", d, err)
		}
	}
	if r := f.run("", "config"); r.code != 0 || !strings.Contains(r.stdout, "user     loaded "+cfgFile) {
		t.Fatalf("the starter file should load: %+v", r)
	}
	if r := f.run("", "init"); !strings.Contains(r.stdout, "already up to date") {
		t.Fatalf("second run: %+v", r)
	}

	mine := filepath.Join(f.home, "mine.yaml")
	os.WriteFile(mine, []byte("version: 1\nexecution:\n  max_concurrent_sessions: 3\n"), 0o644)
	if r := f.run("", "init", "--config", mine); r.code == 0 || !strings.Contains(r.stderr, "--force") {
		t.Fatalf("a different file was replaced without --force: %+v", r)
	}
	if r := f.run("", "init", "--config", mine, "--force"); r.code != 0 || !strings.Contains(r.stdout, "replaced") {
		t.Fatalf("force: %+v", r)
	}
	if b, _ := os.ReadFile(cfgFile + ".bak"); !strings.Contains(string(b), "rbridge configuration") {
		t.Fatal("the old configuration was not kept as a backup")
	}
	if r := f.run("", "init", "--config", mine); r.code != 0 || !strings.Contains(r.stdout, "already up to date") {
		t.Fatalf("same file again: %+v", r)
	}

	bad := filepath.Join(f.home, "bad.yaml")
	os.WriteFile(bad, []byte("execution:\n  resource_ceiling_percent: 500\n"), 0o644)
	if r := f.run("", "init", "--config", bad, "--force"); r.code == 0 || !strings.Contains(r.stderr, "not a valid rbridge configuration") {
		t.Fatalf("bad file: %+v", r)
	}
	if b, _ := os.ReadFile(cfgFile); !strings.Contains(string(b), "max_concurrent_sessions: 3") {
		t.Fatal("a bad file overwrote the configuration")
	}

	skills := t.TempDir()
	for _, name := range []string{"evidence-rules", "draft-notes"} {
		os.Rename(writeSkill(t, name), filepath.Join(skills, name))
	}
	if r := f.run("", "init", "--skills", skills, "--exclude", "draft-*", "--trust", "reviewed"); r.code != 0 || !strings.Contains(r.stdout, "added evidence-rules") || !strings.Contains(r.stdout, "trust reviewed") {
		t.Fatalf("skills: %+v", r)
	}
	if r := f.run("", "init", "--trust", "reviewed"); r.code == 0 {
		t.Fatal("--trust without --skills should fail")
	}
}

func TestUninstall(t *testing.T) {
	f := newFakeCLI(t)
	home := filepath.Join(f.home, "un")
	f.vars["RBRIDGE_HOME"] = home
	// Marked as bundled, so --yes never deletes the test binary itself.
	f.vars["RBRIDGE_BUNDLED"] = "python:/somewhere/.venv"
	if r := f.run("", "init"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	r := f.run("", "uninstall")
	if r.code != 0 || !strings.Contains(r.stdout, "Nothing was changed") || !strings.Contains(r.stdout, "pip uninstall rbridge") {
		t.Fatalf("list: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "config", "config.yaml")); err != nil {
		t.Fatal("listing removed something")
	}
	if r := f.run("", "uninstall", "--yes"); r.code != 0 || !strings.Contains(r.stdout, "were kept") {
		t.Fatalf("yes: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "config")); err != nil {
		t.Fatal("--yes without --purge removed the data")
	}
	if r := f.run("", "uninstall", "--purge", "--yes"); r.code != 0 {
		t.Fatalf("purge: %+v", r)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("home still there after --purge: %v", err)
	}
	if err := safeToRemove(f.home, f.home); err == nil {
		t.Fatal("the home folder itself must never be removed")
	}
	if err := safeToRemove("/", f.home); err == nil {
		t.Fatal("the root must never be removed")
	}
}
