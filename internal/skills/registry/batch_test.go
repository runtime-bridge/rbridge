package registry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

func skillTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	skillDir(t, root, "evidence-rules", "rules")
	skillDir(t, filepath.Join(root, "metrics"), "metric-discovery", "find")
	skillDir(t, filepath.Join(root, "metrics"), "metric-verification", "check")
	// Not skills: a skill's own references, a hidden folder and node_modules.
	os.MkdirAll(filepath.Join(root, "evidence-rules", "references", "nested"), 0o755)
	skillDir(t, filepath.Join(root, "evidence-rules", "references"), "inner-skill", "never found")
	skillDir(t, filepath.Join(root, ".cache"), "hidden-skill", "never found")
	skillDir(t, filepath.Join(root, "node_modules"), "module-skill", "never found")
	return root
}

func statuses(res AddManyResult) map[string]string {
	out := map[string]string{}
	for _, it := range res.Items {
		out[it.ID] = it.Status
	}
	return out
}

func TestFindSkills(t *testing.T) {
	root := skillTree(t)
	found, err := FindSkills(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range found {
		names = append(names, filepath.Base(d))
	}
	if got := strings.Join(names, ","); got != "evidence-rules,metric-discovery,metric-verification" {
		t.Fatalf("found %s", got)
	}
	one, err := FindSkills(filepath.Join(root, "evidence-rules"))
	if err != nil || len(one) != 1 {
		t.Fatalf("a skill folder should be found as itself: %v %v", one, err)
	}
	if _, err := FindSkills(filepath.Join(root, "missing")); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("missing folder: %v", err)
	}
}

func TestAddManyFiltersAndTrust(t *testing.T) {
	ctx := context.Background()
	root := skillTree(t)
	r := newReg(t)

	dry, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}, Exclude: []string{"metric-*"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(dry); s["evidence-rules"] != "would_add" || s["metric-discovery"] != "skipped" || s["metric-verification"] != "skipped" {
		t.Fatalf("dry run: %v", s)
	}
	if list, _ := r.List(ctx); len(list) != 0 {
		t.Fatalf("a dry run installed %d skills", len(list))
	}

	trusted := 0
	only, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}, Only: []string{"metric-*"}, SetTrust: func(ctx context.Context, id string) (domain.SkillRecord, error) {
		trusted++
		return r.SetTrust(ctx, id, domain.TrustReviewed)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(only); s["metric-discovery"] != "added" || s["metric-verification"] != "added" || s["evidence-rules"] != "skipped" || trusted != 2 {
		t.Fatalf("only: %v, trusted %d", s, trusted)
	}

	all, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(all); s["evidence-rules"] != "added" || s["metric-discovery"] != "unchanged" {
		t.Fatalf("all: %v", s)
	}
	rec, _, _ := r.Get(ctx, "metric-discovery")
	if rec.Trust != domain.TrustReviewed {
		t.Fatalf("adding again without --trust changed trust to %s", rec.Trust)
	}

	if _, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}, Only: []string{"nosuch"}}); domain.CodeOf(err) != domain.ErrSkillNotFound {
		t.Fatalf("--only naming nothing: %v", err)
	}
	if _, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}, Exclude: []string{"["}}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("bad pattern: %v", err)
	}
	if res, err := r.AddMany(ctx, AddManyRequest{Paths: []string{root}, Exclude: []string{"zzz"}}); err != nil || len(res.Warnings) != 1 {
		t.Fatalf("unused --exclude should warn: %v %v", res.Warnings, err)
	}
}

func TestAddManyKeepsGoingPastABadSkill(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	skillDir(t, root, "good-one", "ok")
	bad := filepath.Join(root, "bad-one")
	os.MkdirAll(bad, 0o755)
	os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("no frontmatter\n"), 0o644)
	other := t.TempDir()
	skillDir(t, other, "good-one", "a different skill with the same name")

	res, err := newReg(t).AddMany(ctx, AddManyRequest{Paths: []string{root, other}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() != 2 || len(res.Items) != 3 || res.Items[0].Status != "failed" || res.Items[1].Status != "added" {
		t.Fatalf("%+v", res.Items)
	}
	if !strings.Contains(res.Items[2].Reason, "is also skill good-one") {
		t.Fatalf("duplicate name: %+v", res.Items[2])
	}
}

func TestMatchIDs(t *testing.T) {
	ids := []string{"evidence-rules", "metric-discovery", "metric-verification"}
	got, err := MatchIDs(ids, []string{"metric-*", "metric-discovery", "evidence-rules"})
	if err != nil || strings.Join(got, ",") != "metric-discovery,metric-verification,evidence-rules" {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := MatchIDs(ids, []string{"nosuch"}); domain.CodeOf(err) != domain.ErrSkillNotFound {
		t.Fatalf("%v", err)
	}
}

func TestAddManyFromGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file:// URLs are built from POSIX paths here")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := skillTree(t)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	r := newReg(t)
	whole, err := r.AddMany(ctx, AddManyRequest{URL: "file://" + repo, Rev: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if s := statuses(whole); len(s) != 3 || s["metric-verification"] != "added" {
		t.Fatalf("whole repository: %v", s)
	}
	rec, _, _ := r.Get(ctx, "metric-discovery")
	if rec.Source.Subdir != "metrics/metric-discovery" {
		t.Fatalf("subdir recorded as %q", rec.Source.Subdir)
	}
	part, err := newReg(t).AddMany(ctx, AddManyRequest{URL: "file://" + repo, Rev: "main", Subdirs: []string{"metrics"}})
	if err != nil || len(part.Items) != 2 {
		t.Fatalf("subdir: %+v %v", part.Items, err)
	}
	if _, err := newReg(t).AddMany(ctx, AddManyRequest{URL: "file://" + repo, Rev: "main", Subdirs: []string{"nowhere"}}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("missing subdir: %v", err)
	}
}
