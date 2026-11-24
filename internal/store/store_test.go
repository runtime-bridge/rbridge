package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

type doc struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func stores(t *testing.T) map[string]Store {
	js, err := NewJSONStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"json": js, "mem": NewMemStore()}
}

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			var d doc
			if ok, err := s.Get(ctx, "things", "a", &d); ok || err != nil {
				t.Fatalf("missing key: ok=%v err=%v", ok, err)
			}
			if err := s.Put(ctx, "things", "b", doc{"b", 2}); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(ctx, "things", "a", doc{"a", 1}); err != nil {
				t.Fatal(err)
			}
			if err := s.Put(ctx, "things", "a", doc{"a", 3}); err != nil {
				t.Fatal(err)
			}
			if ok, err := s.Get(ctx, "things", "a", &d); !ok || err != nil || d.Count != 3 {
				t.Fatalf("get: ok=%v err=%v d=%+v", ok, err, d)
			}
			keys, err := s.List(ctx, "things")
			if err != nil || !slices.Equal(keys, []string{"a", "b"}) {
				t.Fatalf("list = %v %v", keys, err)
			}
			if err := s.Delete(ctx, "things", "a"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "things", "a"); err != nil {
				t.Fatal("deleting a missing key should not fail")
			}
			keys, _ = s.List(ctx, "things")
			if !slices.Equal(keys, []string{"b"}) {
				t.Fatalf("after delete: %v", keys)
			}
			if keys, err := s.List(ctx, "empty"); err != nil || len(keys) != 0 {
				t.Fatalf("empty collection: %v %v", keys, err)
			}
		})
	}
}

func TestRejectsBadNames(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		for _, bad := range []string{"", "../x", "a/b", ".hidden", "a..b", `a\b`} {
			if err := s.Put(ctx, "c", bad, doc{}); err == nil {
				t.Errorf("%s: key %q accepted", name, bad)
			}
			if err := s.Put(ctx, bad, "k", doc{}); err == nil {
				t.Errorf("%s: collection %q accepted", name, bad)
			}
		}
	}
}

func TestJSONStorePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	root := t.TempDir()
	s, _ := NewJSONStore(root)
	if err := s.Put(context.Background(), "c", "k", doc{}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(root, "c", "k.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Join(root, "c"))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Join(root, "c"))
	if len(entries) != 1 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestJSONStoreNeedsAbsoluteRoot(t *testing.T) {
	if _, err := NewJSONStore("relative"); err == nil {
		t.Fatal("relative root accepted")
	}
}
