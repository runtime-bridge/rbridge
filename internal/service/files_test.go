//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

func TestTaskFiles(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "notes.md"), []byte("# Notes\n"), 0o644)
	docx, _ := filepath.Abs(filepath.Join("..", "files", "testdata", "bill.docx"))

	// No workspace: rbridge makes one holding only the files, and removes it.
	sess, err := h.svc().OpenSession(ctx, domain.Task{Prompt: domain.Text("list them"), Provider: "opencode",
		Files: []string{filepath.Join(src, "notes.md"), docx}}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ws := sess.Record().Workspace
	for _, f := range []string{"notes.md", "bill.docx", "bill.docx.txt"} {
		if _, err := os.Stat(filepath.Join(ws, f)); err != nil {
			t.Fatalf("%s not in the workspace: %v", f, err)
		}
	}
	res, err := sess.Prompt(ctx, domain.Text("hi"))
	if err != nil {
		t.Fatal(err)
	}
	sess.Close(ctx)
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatalf("the files workspace was left behind: %v", err)
	}
	_ = res

	// A run sends the note with the prompt.
	res, err = h.svc().Run(ctx, domain.Task{Prompt: domain.Text("go"), Provider: "opencode", Files: []string{docx}}, RunOptions{})
	if err != nil || !strings.Contains(res.Output, "Files for this task, in your working folder: bill.docx (a Word file: read its text in bill.docx.txt") {
		t.Fatalf("%+v %v", res, err)
	}

	// With a workspace, files go into a folder of their own inside it.
	work := h.workspace()
	res, err = h.svc().Run(ctx, domain.Task{Workspace: work, Prompt: domain.Text("go"), Provider: "opencode", Files: []string{filepath.Join(src, "notes.md")}}, RunOptions{})
	if err != nil || !strings.Contains(res.Output, "in .rbridge-files-") {
		t.Fatalf("%+v %v", res, err)
	}
	if left, _ := filepath.Glob(filepath.Join(work, ".rbridge-files-*")); len(left) > 0 {
		t.Fatalf("placed files left in the workspace: %v", left)
	}

	if _, err := h.svc().Run(ctx, domain.Task{Prompt: domain.Text("go"), Provider: "opencode", Files: []string{filepath.Join(src, "missing.pdf")}}, RunOptions{}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := h.svc().Run(ctx, domain.Task{Prompt: domain.Text("go"), Provider: "opencode", Files: []string{"relative.md"}}, RunOptions{}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Fatalf("relative file: %v", err)
	}
}

func TestSkillFoldersByPath(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	root := t.TempDir()
	for _, name := range []string{"evidence-rules", "line-items"} {
		os.MkdirAll(filepath.Join(root, name), 0o755)
		os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte("---\nname: "+name+"\ndescription: A skill.\n---\nSteps.\n"), 0o644)
	}
	res, err := h.svc().Run(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Skills: []string{root}}, RunOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skills) != 2 {
		t.Fatalf("delivered %+v", res.Skills)
	}
	rec, _, _ := h.svc().Skills().Get(ctx, "evidence-rules")
	if rec.Trust != domain.TrustReviewed {
		t.Fatalf("trust %s", rec.Trust)
	}
	// A decision already made stands.
	h.svc().SetSkillTrust(ctx, "line-items", domain.TrustBlocked)
	if _, err := h.svc().Run(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code", Skills: []string{filepath.Join(root, "line-items")}}, RunOptions{}); domain.CodeOf(err) != domain.ErrSkillBlocked {
		t.Fatalf("a blocked skill was re-trusted: %v", err)
	}
}
