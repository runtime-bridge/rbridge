package spec

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func BenchmarkDigest(b *testing.B) {
	dir := filepath.Join(b.TempDir(), "bench-skill")
	os.MkdirAll(filepath.Join(dir, "references"), 0o755)
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: bench-skill\ndescription: d\n---\n"), 0o644)
	for i := range 50 {
		os.WriteFile(filepath.Join(dir, "references", fmt.Sprintf("r%02d.md", i)), []byte(strings.Repeat("x", 4096)), 0o644)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Digest(dir, DefaultLimits); err != nil {
			b.Fatal(err)
		}
	}
}
