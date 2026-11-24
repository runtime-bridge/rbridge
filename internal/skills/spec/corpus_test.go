package spec

import (
	"os"
	"path/filepath"
	"testing"
)

// The compatibility corpus must stay valid and must not trip any warning.
func TestCorpusIsValid(t *testing.T) {
	root := filepath.Join("..", "..", "..", "testdata", "skills")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("expected five corpus skills, found %d", len(entries))
	}
	for _, e := range entries {
		sk, err := Parse(filepath.Join(root, e.Name()), DefaultLimits)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if len(sk.Warnings) > 0 {
			t.Errorf("%s: warnings %v", e.Name(), sk.Warnings)
		}
		if sk.Metadata["corpus"] != "rbridge-compat" {
			t.Errorf("%s: not marked as corpus", e.Name())
		}
	}
}
