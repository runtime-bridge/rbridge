package files

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDocxText(t *testing.T) {
	text, err := DocxText(filepath.Join("testdata", "bill.docx"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(text, "--- Page "); n != 5 {
		t.Fatalf("%d pages", n)
	}
	if !strings.Contains(text, "| 1/D | Mass concrete foundations, C25/30, poured against earth faces | 54 | m3 | 145.00 | 7,830.00 |") {
		t.Fatalf("table row missing:\n%s", text[:600])
	}
	if !strings.Contains(text, "--- Page 5 ---\nGeneral summary") {
		t.Fatal("page 5 should start with the general summary")
	}
}

func TestPlace(t *testing.T) {
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "notes.md"), []byte("# Notes\n"), 0o644)
	dir := t.TempDir()
	placed, err := Place(dir, []string{filepath.Join(src, "notes.md"), filepath.Join("testdata", "bill.docx")})
	if err != nil {
		t.Fatal(err)
	}
	if len(placed) != 2 || placed[0].Text != "" || placed[1].Text != "bill.docx.txt" {
		t.Fatalf("%+v", placed)
	}
	for _, f := range []string{"notes.md", "bill.docx", "bill.docx.txt"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("%s not placed: %v", f, err)
		}
	}
	note := Note(placed)
	if !strings.Contains(note, "notes.md; bill.docx (a Word file: read its text in bill.docx.txt") {
		t.Fatalf("note %q", note)
	}
	if _, err := Place(t.TempDir(), []string{filepath.Join(src, "notes.md"), filepath.Join(src, "notes.md")}); err == nil {
		t.Fatal("two files with one name were accepted")
	}
	if _, err := Place(t.TempDir(), []string{filepath.Join(src, "missing.pdf")}); err == nil {
		t.Fatal("a missing file was accepted")
	}
	bad := filepath.Join(src, "bad.docx")
	os.WriteFile(bad, []byte("not a zip"), 0o644)
	if _, err := Place(t.TempDir(), []string{bad}); err == nil || !strings.Contains(err.Error(), "not a readable Word document") {
		t.Fatalf("bad docx: %v", err)
	}
}
