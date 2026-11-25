package lockfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func write(t *testing.T, root string, files map[string]string) string {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func hash(t *testing.T, dir string) string {
	t.Helper()
	h, err := Hash(dir)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// The expected values are the test vectors published with the lock format.
func TestHashVectors(t *testing.T) {
	empty := t.TempDir()
	if got := hash(t, empty); got != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("empty: %s", got)
	}
	one := write(t, t.TempDir(), map[string]string{"SKILL.md": "# hi\n"})
	if got := hash(t, one); got != "93c36952d8843b33e68ae970ebabcc03e1687c9bb0dd1ecaf357060318952f55" {
		t.Errorf("one file: %s", got)
	}
	two := write(t, t.TempDir(), map[string]string{"docs/b.md": "b\n", "SKILL.md": "a\n"})
	const twoHash = "b22c4d22aa319d2924312fa18fcbfedc88fa89c74dbd8f298c683e1caf7b7f24"
	if got := hash(t, two); got != twoHash {
		t.Errorf("two files: %s", got)
	}

	// Excluded names, CRLF text and links inside the folder do not change it.
	same := write(t, t.TempDir(), map[string]string{
		"docs/b.md": "b\r\n", "SKILL.md": "a\r\n", "metadata.json": "{}", "docs/metadata.json": "{}",
		".git/HEAD": "x", "__pycache__/c.pyc": "y",
	})
	if got := hash(t, same); got != twoHash {
		t.Errorf("normalised: %s", got)
	}
	if runtime.GOOS != "windows" {
		outside := write(t, t.TempDir(), map[string]string{"secret.txt": "s"})
		os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(same, "escape.txt"))
		os.Symlink(same, filepath.Join(same, "docs", "loop"))
		if got := hash(t, same); got != twoHash {
			t.Errorf("escaping link or cycle changed the hash: %s", got)
		}
		linked := write(t, t.TempDir(), map[string]string{"real/b.md": "b\n", "SKILL.md": "a\n"})
		os.Symlink("real", filepath.Join(linked, "docs"))
		os.RemoveAll(filepath.Join(linked, "real"))
		write(t, linked, map[string]string{"store/b.md": "b\n"})
		os.Remove(filepath.Join(linked, "docs"))
		os.Symlink("store", filepath.Join(linked, "docs"))
		// docs -> store inside the folder: both docs/b.md and store/b.md count.
		want := write(t, t.TempDir(), map[string]string{"docs/b.md": "b\n", "store/b.md": "b\n", "SKILL.md": "a\n"})
		if hash(t, linked) != hash(t, want) {
			t.Error("a link inside the folder must count as its content")
		}
	}

	// Binary files keep CRLF.
	bin1 := write(t, t.TempDir(), map[string]string{"a.bin": "\x00\r\n"})
	bin2 := write(t, t.TempDir(), map[string]string{"a.bin": "\x00\n"})
	if hash(t, bin1) == hash(t, bin2) {
		t.Error("CRLF in a binary file must not be normalised")
	}
}

func TestRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, Name)
	os.WriteFile(p, []byte(`{"version":2,"skills":{"pdf":{"source":"org/skills","sourceType":"github","commit":"0123456789abcdef0123456789abcdef01234567","skillPath":"skills/pdf/SKILL.md","computedHash":"abc"}}}`), 0o644)
	l, err := Read(p)
	if err != nil {
		t.Fatal(err)
	}
	url, sub, ok := l.Skills["pdf"].GitURL()
	if !ok || url != "https://github.com/org/skills.git" || sub != "skills/pdf" {
		t.Fatalf("%s %s %v", url, sub, ok)
	}
	os.WriteFile(p, []byte(`{"version":1,"skills":{"a":{"source":"x","sourceType":"local","computedHash":"old"}}}`), 0o644)
	if l, err := Read(p); err != nil || l.Skills["a"].ComputedHash != "" {
		t.Fatalf("v1 hashes must be dropped: %+v %v", l, err)
	}
	for _, bad := range []string{`{"version":3,"skills":{}}`, `{"version":2}`, "<<<<<<< HEAD\n{}", "not json"} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Read(p); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if _, _, ok := (Entry{SourceType: "github", Source: "org/x"}).GitURL(); ok {
		t.Error("an entry without a commit is not pinned")
	}
	if _, _, ok := (Entry{SourceType: "local", Source: "./skills", Commit: "abc"}).GitURL(); ok {
		t.Error("local entries have no Git URL")
	}
}
