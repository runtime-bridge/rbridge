package spec

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func mk(t *testing.T, name, skillMD string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if skillMD != "" {
		os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(skillMD), 0o644)
	}
	for p, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(body), 0o644)
	}
	return dir
}

const good = "---\nname: code-review\ndescription: Review a diff.\nlicense: Apache-2.0\nmetadata:\n  version: \"1.2.0\"\n---\n\n# Review\n"

func TestParseValid(t *testing.T) {
	dir := mk(t, "code-review", good, map[string]string{"references/list.md": "x", "scripts/run.sh": "echo"})
	sk, err := Parse(dir, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "code-review" || sk.Version() != "1.2.0" || !strings.HasPrefix(sk.Digest, "sha256:") {
		t.Fatalf("%+v", sk)
	}
	if !slices.Equal(sk.Files, []string{"SKILL.md", "references/list.md", "scripts/run.sh"}) {
		t.Fatalf("files %v", sk.Files)
	}
	if !slices.Equal(sk.Scripts, []string{"scripts/run.sh"}) || !sk.HasScripts() {
		t.Fatalf("scripts %v", sk.Scripts)
	}
	if !strings.HasPrefix(sk.Body, "\n# Review") && !strings.HasPrefix(sk.Body, "# Review") {
		t.Fatalf("body %q", sk.Body)
	}
}

func TestParseProblems(t *testing.T) {
	cases := map[string]struct {
		dir, md string
		want    string
	}{
		"missing file":     {"a", "", "SKILL.md is missing"},
		"no frontmatter":   {"a", "# hi", "must start with YAML frontmatter"},
		"unclosed":         {"a", "---\nname: a\n", "not closed"},
		"no name":          {"a", "---\ndescription: d\n---\n", "name is required"},
		"bad name":         {"Bad_Name", "---\nname: Bad_Name\ndescription: d\n---\n", "lowercase"},
		"double hyphen":    {"a--b", "---\nname: a--b\ndescription: d\n---\n", "lowercase"},
		"dir mismatch":     {"other", "---\nname: code\ndescription: d\n---\n", "must match the directory"},
		"no description":   {"a", "---\nname: a\n---\n", "description is required"},
		"long description": {"a", "---\nname: a\ndescription: " + strings.Repeat("x", 1025) + "\n---\n", "longer than 1024"},
		"metadata not map": {"a", "---\nname: a\ndescription: d\nmetadata: [1]\n---\n", "metadata must be a map"},
		"metadata value":   {"a", "---\nname: a\ndescription: d\nmetadata:\n  n: 3\n---\n", "metadata.n must be a string"},
		"bad yaml":         {"a", "---\nname: [\n---\n", "not valid YAML"},
		"compat empty":     {"a", "---\nname: a\ndescription: d\ncompatibility: \"\"\n---\n", "compatibility must be"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(mk(t, c.dir, c.md, nil), DefaultLimits)
			var ve *ValidationError
			if !errors.As(err, &ve) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

func TestWarnings(t *testing.T) {
	sk, err := Parse(mk(t, "a", "---\nname: a\ndescription: d\nfancy: yes\n---\n", nil), DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sk.Warnings, ";")
	if !strings.Contains(joined, "unknown frontmatter field fancy") || !strings.Contains(joined, "no license") {
		t.Fatalf("warnings %v", sk.Warnings)
	}
}

func TestSymlinkRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := mk(t, "a", "---\nname: a\ndescription: d\n---\n", map[string]string{"references/real.md": "x"})
	os.Symlink("real.md", filepath.Join(dir, "references", "alias.md"))
	if _, err := Parse(dir, DefaultLimits); err != nil {
		t.Fatalf("internal relative symlink rejected: %v", err)
	}
	os.Symlink("../../outside", filepath.Join(dir, "references", "escape.md"))
	if _, err := Parse(dir, DefaultLimits); err == nil || !strings.Contains(err.Error(), "outside the skill") {
		t.Fatalf("escape accepted: %v", err)
	}
	os.Remove(filepath.Join(dir, "references", "escape.md"))
	os.Symlink("/etc/passwd", filepath.Join(dir, "abs"))
	if _, err := Parse(dir, DefaultLimits); err == nil || !strings.Contains(err.Error(), "absolute target") {
		t.Fatalf("absolute symlink accepted: %v", err)
	}
}

func TestLimits(t *testing.T) {
	dir := mk(t, "a", "---\nname: a\ndescription: d\n---\n", map[string]string{"big.bin": strings.Repeat("x", 2000)})
	if _, err := Parse(dir, Limits{MaxFileBytes: 1000, MaxTotalBytes: 1 << 20, MaxFiles: 100}); err == nil {
		t.Fatal("file size limit not applied")
	}
	if _, err := Parse(dir, Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1000, MaxFiles: 100}); err == nil {
		t.Fatal("total size limit not applied")
	}
	if _, err := Parse(dir, Limits{MaxFileBytes: 1 << 20, MaxTotalBytes: 1 << 20, MaxFiles: 1}); err == nil {
		t.Fatal("file count limit not applied")
	}
}

func TestDigestStableAndSensitive(t *testing.T) {
	a := mk(t, "a", good[:0]+"---\nname: a\ndescription: d\n---\n", map[string]string{"x/y.md": "1", "z.md": "2"})
	d1, err := Digest(a, DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	// Same content elsewhere gives the same digest; mtimes do not matter.
	b := mk(t, "a", "---\nname: a\ndescription: d\n---\n", map[string]string{"z.md": "2", "x/y.md": "1"})
	if d2, _ := Digest(b, DefaultLimits); d2 != d1 {
		t.Fatal("digest depends on location or creation order")
	}
	os.WriteFile(filepath.Join(a, "z.md"), []byte("3"), 0o644)
	if d3, _ := Digest(a, DefaultLimits); d3 == d1 {
		t.Fatal("content change not detected")
	}
	os.WriteFile(filepath.Join(a, "z.md"), []byte("2"), 0o644)
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(a, "z.md"), 0o755)
		if d4, _ := Digest(a, DefaultLimits); d4 == d1 {
			t.Fatal("executable bit change not detected")
		}
	}
}

func TestCRLFAndBOM(t *testing.T) {
	md := "\xef\xbb\xbf---\r\nname: a\r\ndescription: d\r\n---\r\nbody\r\n"
	if _, err := Parse(mk(t, "a", md, nil), DefaultLimits); err != nil {
		t.Fatal(err)
	}
}
