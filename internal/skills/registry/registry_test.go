package registry

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/skills/spec"
	"github.com/rbridge/rbridge/internal/store"
)

func newReg(t *testing.T) *Registry {
	return &Registry{Dir: filepath.Join(t.TempDir(), "skills"), Store: store.NewMemStore(), Limits: spec.DefaultLimits}
}

func skillDir(t *testing.T, root, name, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	os.MkdirAll(filepath.Join(dir, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: Test skill.\nlicense: MIT\n---\n"+body), 0o644)
	os.WriteFile(filepath.Join(dir, "scripts", "run.sh"), []byte("echo hi\n"), 0o755)
	return dir
}

func TestAddLocalCopy(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	src := skillDir(t, t.TempDir(), "doc-writer", "v1")
	res, err := r.Add(ctx, AddRequest{Path: src})
	if err != nil {
		t.Fatal(err)
	}
	rec := res.Record
	if rec.Trust != domain.TrustUnreviewed || !rec.HasScripts || rec.Source.Type != "local" {
		t.Fatalf("%+v", rec)
	}
	p := r.Path(rec)
	if filepath.Base(p) != "doc-writer" || !strings.HasPrefix(p, r.Dir) {
		t.Fatalf("library path %s", p)
	}
	if _, err := os.Stat(filepath.Join(p, "scripts", "run.sh")); err != nil {
		t.Fatal("copy incomplete")
	}
	// Changing the source does not affect the library copy.
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: doc-writer\ndescription: changed\n---\n"), 0o644)
	if err := r.Verify(ctx, rec); err != nil {
		t.Fatalf("library copy should be unaffected: %v", err)
	}
}

func TestReAddKeepsOrResetsTrust(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	src := skillDir(t, t.TempDir(), "code-review", "v1")
	first, _ := r.Add(ctx, AddRequest{Path: src})
	if _, err := r.SetTrust(ctx, "code-review", domain.TrustTrusted); err != nil {
		t.Fatal(err)
	}
	again, err := r.Add(ctx, AddRequest{Path: src})
	if err != nil || !again.Unchanged || again.Record.Trust != domain.TrustTrusted {
		t.Fatalf("same content: %+v %v", again, err)
	}
	os.WriteFile(filepath.Join(src, "SKILL.md"), []byte("---\nname: code-review\ndescription: v2\n---\nnew"), 0o644)
	next, err := r.Add(ctx, AddRequest{Path: src})
	if err != nil {
		t.Fatal(err)
	}
	if next.Record.Trust != domain.TrustUnreviewed || next.Replaced != first.Record.Digest {
		t.Fatalf("new content must reset trust: %+v", next)
	}
	if _, err := os.Stat(r.Path(first.Record)); !os.IsNotExist(err) {
		t.Fatal("old library copy left behind")
	}
}

func TestLinkedSkillDetectsChanges(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	src := skillDir(t, t.TempDir(), "report-builder", "v1")
	res, err := r.Add(ctx, AddRequest{Path: src, Link: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Path(res.Record) != src {
		t.Fatal("linked skill should be used in place")
	}
	os.WriteFile(filepath.Join(src, "extra.md"), []byte("x"), 0o644)
	if domain.CodeOf(r.Verify(ctx, res.Record)) != domain.ErrSkillDigestMismatch {
		t.Fatal("change in linked source not detected")
	}
	if _, err := r.SetTrust(ctx, "report-builder", domain.TrustTrusted); domain.CodeOf(err) != domain.ErrSkillDigestMismatch {
		t.Fatalf("trusting changed content must fail: %v", err)
	}
	if _, err := r.SetTrust(ctx, "report-builder", domain.TrustBlocked); err != nil {
		t.Fatalf("blocking is always allowed: %v", err)
	}
	if err := r.Remove(ctx, "report-builder"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal("removing a linked skill must not delete its source")
	}
}

func TestInvalidAndMissing(t *testing.T) {
	r := newReg(t)
	ctx := context.Background()
	bad := filepath.Join(t.TempDir(), "bad")
	os.MkdirAll(bad, 0o755)
	if _, err := r.Add(ctx, AddRequest{Path: bad}); domain.CodeOf(err) != domain.ErrSkillInvalid {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Require(ctx, "nope"); domain.CodeOf(err) != domain.ErrSkillNotFound {
		t.Fatalf("got %v", err)
	}
	if _, err := r.Add(ctx, AddRequest{}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("got %v", err)
	}
	if _, err := ParseTrust("maybe"); err == nil {
		t.Fatal("bad trust accepted")
	}
}

func TestAddGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file:// URLs are built from POSIX paths here")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	skillDir(t, filepath.Join(repo, "skills"), "project-stats", "from git")
	run := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("add", ".")
	run("commit", "-q", "-m", "init")
	commit := run("rev-parse", "HEAD")

	r := newReg(t)
	r.RequirePinned = true
	url := "file://" + repo
	if _, err := r.Add(ctx, AddRequest{URL: url, Rev: "main", Subdir: "skills/project-stats"}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("branch accepted while pinning is required: %v", err)
	}
	res, err := r.Add(ctx, AddRequest{URL: url, Rev: commit, Subdir: "skills/project-stats"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Record.Source.Type != "git" || res.Record.Source.Revision != commit {
		t.Fatalf("%+v", res.Record.Source)
	}
	if err := r.Verify(ctx, res.Record); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Add(ctx, AddRequest{URL: url, Rev: commit, Subdir: "../x"}); err == nil {
		t.Fatal("subdir escape accepted")
	}
	if _, err := r.Add(ctx, AddRequest{URL: "http://insecure/x", Rev: commit}); err == nil {
		t.Fatal("plain http accepted")
	}
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".git-") {
			t.Fatal("temporary clone left behind")
		}
	}
}

func TestCopySkillSkip(t *testing.T) {
	src := skillDir(t, t.TempDir(), "a", "x")
	dest := filepath.Join(t.TempDir(), "out", "a")
	if err := CopySkill(src, dest, func(rel string) bool { return rel == "scripts" || strings.HasPrefix(rel, "scripts/") }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dest, "SKILL.md")); err != nil {
		t.Fatal("SKILL.md missing")
	}
	if _, err := os.Stat(filepath.Join(dest, "scripts")); !os.IsNotExist(err) {
		t.Fatal("scripts copied despite skip")
	}
}
