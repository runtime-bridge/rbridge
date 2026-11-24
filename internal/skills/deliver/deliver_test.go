package deliver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbridge/rbridge/internal/domain"
)

func skill(t *testing.T, name string) Item {
	dir := filepath.Join(t.TempDir(), name)
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.MkdirAll(filepath.Join(dir, "references"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: d\n---\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "references", "r.md"), []byte("ref"), 0o644)
	os.WriteFile(filepath.Join(dir, "scripts", "s.sh"), []byte("echo"), 0o755)
	os.WriteFile(filepath.Join(dir, "tool"), []byte("#!/bin/sh"), 0o755)
	return Item{Record: domain.SkillRecord{ID: name, Digest: "sha256:x"}, Dir: dir, Scripts: true, ScriptFiles: []string{"scripts/s.sh", "tool"}}
}

func TestSessionDir(t *testing.T) {
	scratch := t.TempDir()
	plan, err := Prepare(Request{Strategy: SessionDir, Layout: ".claude/skills", Scratch: scratch, Items: []Item{skill(t, "a")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.SkillDirs) != 1 || plan.SkillDirs[0] != filepath.Join(scratch, "skills-session") {
		t.Fatalf("dirs %v", plan.SkillDirs)
	}
	if _, err := os.Stat(filepath.Join(scratch, "skills-session", ".claude", "skills", "a", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plan.Deliveries[0].Mode, "session-dir") || !plan.Scripts["a"] {
		t.Fatalf("%+v", plan)
	}
}

func TestScriptsWithheld(t *testing.T) {
	scratch := t.TempDir()
	it := skill(t, "a")
	it.Scripts = false
	plan, err := Prepare(Request{Strategy: SessionDir, Layout: ".claude/skills", Scratch: scratch, Items: []Item{it}})
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(scratch, "skills-session", ".claude", "skills", "a")
	if _, err := os.Stat(filepath.Join(base, "references", "r.md")); err != nil {
		t.Fatal("references must be delivered")
	}
	for _, p := range []string{"scripts", "tool"} {
		if _, err := os.Stat(filepath.Join(base, p)); !os.IsNotExist(err) {
			t.Fatalf("%s delivered although scripts are withheld", p)
		}
	}
	if fi, _ := os.Lstat(base); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("withheld delivery must be a copy, not a link")
	}
	if plan.Scripts["a"] || !strings.HasSuffix(plan.Deliveries[0].Mode, "+no-scripts") {
		t.Fatalf("%+v", plan)
	}
}

func TestOpenCodeConfig(t *testing.T) {
	scratch := t.TempDir()
	env := map[string]string{}
	plan, err := Prepare(Request{Strategy: OpenCodeConfig, Scratch: scratch, Items: []Item{skill(t, "a")}, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	cfg := plan.Env["OPENCODE_CONFIG"]
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Skills struct{ Paths []string } `json:"skills"`
	}
	json.Unmarshal(b, &c)
	if len(c.Skills.Paths) != 1 {
		t.Fatalf("config %s", b)
	}
	if _, err := os.Stat(filepath.Join(c.Skills.Paths[0], "a", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	env["OPENCODE_CONFIG"] = "/user/own.json"
	plan, err = Prepare(Request{Strategy: OpenCodeConfig, Scratch: t.TempDir(), Items: []Item{skill(t, "b")}, Getenv: func(k string) string { return env[k] }})
	if err != nil || plan.Env["OPENCODE_CONFIG"] != "" || plan.Env["OPENCODE_CONFIG_DIR"] == "" {
		t.Fatalf("should fall back to OPENCODE_CONFIG_DIR: %+v %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(plan.Env["OPENCODE_CONFIG_DIR"], "skills", "b", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	env["OPENCODE_CONFIG_DIR"] = "/user/dir"
	_, err = Prepare(Request{Strategy: OpenCodeConfig, Scratch: t.TempDir(), Items: []Item{skill(t, "c")}, Getenv: func(k string) string { return env[k] }})
	if domain.CodeOf(err) != domain.ErrSkillUnavailable {
		t.Fatalf("both set: %v", err)
	}
}

func TestNoStrategy(t *testing.T) {
	if _, err := Prepare(Request{Strategy: "", Scratch: t.TempDir(), Items: []Item{skill(t, "a")}}); domain.CodeOf(err) != domain.ErrSkillUnavailable {
		t.Fatalf("got %v", err)
	}
	if plan, err := Prepare(Request{Strategy: ""}); err != nil || len(plan.Deliveries) != 0 {
		t.Fatal("no items should be a no-op")
	}
}

func TestFindClashes(t *testing.T) {
	ws := t.TempDir()
	user := t.TempDir()
	os.MkdirAll(filepath.Join(ws, ".claude", "skills", "a"), 0o755)
	os.WriteFile(filepath.Join(ws, ".claude", "skills", "a", "SKILL.md"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(user, "a"), 0o755)
	os.WriteFile(filepath.Join(user, "a", "SKILL.md"), []byte("x"), 0o644)
	got := FindClashes([]string{"a", "b"}, ws, []string{".claude/skills"}, []string{user})
	if len(got) != 2 {
		t.Fatalf("clashes %+v", got)
	}
}
