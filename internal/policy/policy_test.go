package policy

import (
	"encoding/json"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

func req(kind string, input map[string]any) domain.ApprovalRequest {
	b, _ := json.Marshal(input)
	return domain.ApprovalRequest{Kind: kind, Input: b, Title: kind}
}

func TestBuiltinProfiles(t *testing.T) {
	ws := filepath.FromSlash("/work/proj")
	ctx := Context{Workspace: ws}
	in := map[string]any{"file_path": filepath.Join(ws, "a.go")}
	cases := []struct {
		profile, kind string
		input         map[string]any
		want          Action
	}{
		{"observe", "read", in, Allow},
		{"observe", "edit", in, Deny},
		{"observe", "execute", map[string]any{"command": "ls"}, Deny},
		{"observe", "fetch", nil, Deny},
		{"standard", "read", in, Allow},
		{"standard", "edit", in, Allow},
		{"standard", "delete", in, Ask},
		{"standard", "execute", map[string]any{"command": "go test"}, Ask},
		{"standard", "mystery", nil, Ask},
		{"controlled", "edit", in, Ask},
		{"controlled", "read", in, Allow},
	}
	for _, c := range cases {
		p, ok := Builtin(c.profile)
		if !ok {
			t.Fatal(c.profile)
		}
		if got, _ := p.Decide(req(c.kind, c.input), ctx); got != c.want {
			t.Errorf("%s %s: got %s want %s", c.profile, c.kind, got, c.want)
		}
	}
	if _, ok := Builtin("nope"); ok {
		t.Fatal("unknown profile accepted")
	}
}

// root is an absolute filesystem root on any platform.
func root() string {
	if runtime.GOOS == "windows" {
		return `C:\`
	}
	return "/"
}

func TestPathRules(t *testing.T) {
	ws := filepath.Join(root(), "work", "proj")
	skill := filepath.Join(root(), "run", "rbridge", "sessions", "s1", "skills-session", ".claude", "skills", "sec")
	ctx := Context{Workspace: ws, Skills: map[string]domain.TrustState{skill: domain.TrustReviewed}}
	p, _ := Builtin("standard")
	check := func(kind, path string, want Action) {
		t.Helper()
		if got, why := p.Decide(req(kind, map[string]any{"file_path": path}), ctx); got != want {
			t.Errorf("%s %s: got %s (%s) want %s", kind, path, got, why, want)
		}
	}
	check("edit", filepath.Join(root(), "etc", "passwd"), Deny)
	check("edit", filepath.Join(ws, "..", "other", "x"), Deny)
	check("edit", filepath.Join(skill, "SKILL.md"), Deny)
	check("read", filepath.Join(skill, "references", "checklist.md"), Allow)
	check("read", filepath.Join(root(), "etc", "hosts"), Ask)
	check("edit", "relative/inside.go", Allow)
	obs, _ := Builtin("observe")
	if got, _ := obs.Decide(req("read", map[string]any{"path": filepath.Join(root(), "etc", "hosts")}), ctx); got != Deny {
		t.Errorf("observe outside read: %s", got)
	}
}

func TestSkillScripts(t *testing.T) {
	trusted := filepath.FromSlash("/s/trusted-skill")
	reviewed := filepath.FromSlash("/s/reviewed-skill")
	ctx := Context{Workspace: "/w", Skills: map[string]domain.TrustState{trusted: domain.TrustTrusted, reviewed: domain.TrustReviewed}}
	std, _ := Builtin("standard")
	run := func(p Profile, dir string) Action {
		a, _ := p.Decide(req("execute", map[string]any{"command": "sh " + filepath.Join(dir, "scripts", "x.sh")}), ctx)
		return a
	}
	if run(std, trusted) != Ask {
		t.Error("trusted script under standard follows the execute rule (ask)")
	}
	if run(std, reviewed) != Ask {
		t.Error("reviewed script must ask")
	}
	custom, err := Custom(map[string]string{"execute": "allow"}, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if run(custom, trusted) != Allow || run(custom, reviewed) != Ask {
		t.Error("custom execute allow: trusted runs, reviewed still asks")
	}
	obs, _ := Builtin("observe")
	if run(obs, trusted) != Deny {
		t.Error("observe denies all commands")
	}
}

func TestCustomProfile(t *testing.T) {
	p, err := Custom(map[string]string{"edit": "deny", "execute": "deny", "fetch": "deny"}, "deny", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.Rules["read"] != Ask || p.OutsideRead != Deny || len(p.ClaudeDisallowed) != 7 {
		t.Fatalf("%+v", p)
	}
	if _, err := Custom(map[string]string{"teleport": "allow"}, "", false, false); err == nil {
		t.Fatal("unknown kind accepted")
	}
	if _, err := Custom(map[string]string{"read": "maybe"}, "", false, false); err == nil {
		t.Fatal("bad action accepted")
	}
}

func TestEnforcement(t *testing.T) {
	obs, _ := Builtin("observe")
	c := Enforcement(obs, domain.IntegrationClaudeHeadless)
	found := false
	for _, x := range c {
		if x.Name == "disallowed tools" && x.Status == "enforced" {
			found = true
		}
	}
	if !found {
		t.Fatalf("observe on Claude must remove tools: %+v", c)
	}
	if u := Unenforced(obs, c); len(u) != 1 || u[0] != "network" {
		t.Fatalf("observe wants network blocked, which is not enforced: %v", u)
	}
	std, _ := Builtin("standard")
	if u := Unenforced(std, Enforcement(std, domain.IntegrationACP)); len(u) != 0 {
		t.Fatalf("standard asks for nothing unenforceable: %v", u)
	}
}
