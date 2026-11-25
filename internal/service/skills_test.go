//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
)

func TestPermanentDelivery(t *testing.T) {
	h := newRunHarness(t)
	svc := h.svc()
	ctx := context.Background()
	addSkill(t, svc, "lint-rules", domain.TrustUnreviewed)

	// User folder: scripts are left out until the skill is trusted.
	d, err := svc.DeliverPermanent(ctx, "lint-rules", "claude-code", "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(h.home, ".claude", "skills", "lint-rules")
	if d.Path != want || d.Mode != "copy-no-scripts" {
		t.Fatalf("%+v", d)
	}
	if _, err := os.Stat(filepath.Join(want, "scripts")); !os.IsNotExist(err) {
		t.Fatal("scripts delivered for an unreviewed skill")
	}
	if _, err := svc.SetSkillTrust(ctx, "lint-rules", domain.TrustTrusted); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(want); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("trusting the skill must relink it with its scripts")
	}

	// Project folder in a Git work tree: the path is excluded from Git.
	proj := h.workspace()
	os.MkdirAll(filepath.Join(proj, ".git", "info"), 0o755)
	os.WriteFile(filepath.Join(proj, ".git", "info", "exclude"), []byte("# existing"), 0o644)
	pd, err := svc.DeliverPermanent(ctx, "lint-rules", "opencode", proj)
	if err != nil {
		t.Fatal(err)
	}
	if pd.Path != filepath.Join(proj, ".opencode", "skills", "lint-rules") || pd.Excluded != "/.opencode/skills/lint-rules" {
		t.Fatalf("%+v", pd)
	}
	ex, _ := os.ReadFile(filepath.Join(proj, ".git", "info", "exclude"))
	if string(ex) != "# existing\n/.opencode/skills/lint-rules\n" {
		t.Fatalf("exclude %q", ex)
	}
	// Delivering again is an update, not a clash.
	if _, err := svc.DeliverPermanent(ctx, "lint-rules", "opencode", proj); err != nil {
		t.Fatal(err)
	}
	if list, _ := svc.PermanentDeliveries(ctx, ""); len(list) != 2 {
		t.Fatalf("tracked %+v", list)
	}

	// A folder rbridge did not create is never replaced.
	addSkill(t, svc, "mine", domain.TrustTrusted)
	own := filepath.Join(h.home, ".claude", "skills", "mine")
	os.MkdirAll(own, 0o755)
	os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("the user's own"), 0o644)
	if _, err := svc.DeliverPermanent(ctx, "mine", "claude-code", ""); err == nil || !strings.Contains(err.Error(), "not created by rbridge") {
		t.Fatalf("clash: %v", err)
	}

	// Undeliver one provider, then blocking removes the rest.
	removed, err := svc.Undeliver(ctx, "lint-rules", "opencode", "")
	if err != nil || len(removed) != 1 {
		t.Fatalf("%v %v", removed, err)
	}
	if _, err := os.Lstat(pd.Path); !os.IsNotExist(err) {
		t.Fatal("project copy left behind")
	}
	if ex, _ := os.ReadFile(filepath.Join(proj, ".git", "info", "exclude")); string(ex) != "# existing\n" {
		t.Fatalf("exclude after undeliver %q", ex)
	}
	if _, err := svc.SetSkillTrust(ctx, "lint-rules", domain.TrustBlocked); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(want); !os.IsNotExist(err) {
		t.Fatal("blocked skill still delivered")
	}
	if _, err := svc.DeliverPermanent(ctx, "lint-rules", "claude-code", ""); domain.CodeOf(err) != domain.ErrSkillBlocked {
		t.Fatalf("blocked: %v", err)
	}

	// Removing a skill removes its deliveries.
	svc.SetSkillTrust(ctx, "lint-rules", domain.TrustTrusted)
	if _, err := svc.DeliverPermanent(ctx, "lint-rules", "claude-code", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveSkill(ctx, "lint-rules"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(want); !os.IsNotExist(err) {
		t.Fatal("removed skill still delivered")
	}
	if b, _ := os.ReadFile(filepath.Join(own, "SKILL.md")); string(b) != "the user's own" {
		t.Fatal("the user's own skill was touched")
	}
}

func TestProjectLinkDelivery(t *testing.T) {
	bin := buildFakeAgent(t)
	h := newRunHarness(t)
	h.cfg.Providers.Custom = []config.CustomProvider{{
		ID: "linked", Launch: config.LaunchConfig{Command: bin, EnvPassthrough: []string{fakeacp.EnvScenario}},
		SkillsDir: ".agents/skills",
	}}
	h.env[fakeacp.EnvScenario] = "echo"
	svc := h.svc()
	ctx := context.Background()
	addSkill(t, svc, "api-style", domain.TrustReviewed)
	ws := h.workspace()
	os.MkdirAll(filepath.Join(ws, ".git", "info"), 0o755)
	link := filepath.Join(ws, ".agents", "skills", "api-style")
	exclude := filepath.Join(ws, ".git", "info", "exclude")

	// A link left by a session that died is cleaned up first.
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.Symlink(t.TempDir(), link)
	if err := svc.trackProjectLinks(ctx, "ses_gone", ws, []string{link}); err != nil {
		t.Fatal(err)
	}

	task := domain.Task{Workspace: ws, Prompt: domain.Text("x"), Provider: "linked", Skills: []string{"api-style"}}
	sess, err := svc.OpenSession(ctx, task, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(link, "SKILL.md")); err != nil {
		t.Fatalf("skill not visible in the project during the session: %v", err)
	}
	if d := sess.Record().Skills; len(d) != 1 || !strings.HasPrefix(d[0].Mode, "project-link") {
		t.Fatalf("deliveries %+v", d)
	}
	if ex, _ := os.ReadFile(exclude); string(ex) != "/.agents/skills/api-style\n" {
		t.Fatalf("exclude during session %q", ex)
	}
	if res, err := sess.Prompt(ctx, domain.Text("hi")); err != nil || res.Output != "echo: hi" {
		t.Fatalf("%+v %v", res, err)
	}
	sess.Close(ctx)
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatal("link left in the project after the session")
	}
	if ex, _ := os.ReadFile(exclude); string(ex) != "" {
		t.Fatalf("exclude after session %q", ex)
	}

	// The project's own skill of the same name is never replaced.
	os.MkdirAll(link, 0o755)
	os.WriteFile(filepath.Join(link, "SKILL.md"), []byte("project's own"), 0o644)
	if _, err := svc.OpenSession(ctx, task, RunOptions{}); domain.CodeOf(err) != domain.ErrSkillUnavailable {
		t.Fatalf("clash: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(link, "SKILL.md")); string(b) != "project's own" {
		t.Fatal("project skill touched")
	}
}
