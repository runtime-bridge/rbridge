package audit

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestAppendReadAndRetention(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	l := &Log{Dir: dir, Retention: 60 * 24 * time.Hour, Now: func() time.Time { return now }}
	old := Record{ID: "a", SessionID: "s1", Time: time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)}
	if err := l.Append(old); err != nil {
		t.Fatal(err)
	}
	if err := l.Append(Record{ID: "b", SessionID: "s2", Approvals: []Approval{{Kind: "edit", Decision: "allow", By: "policy"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "2026-08.jsonl")); !os.IsNotExist(err) {
		t.Fatal("month past retention should be pruned")
	}
	recs, err := l.Read(time.Time{})
	if err != nil || len(recs) != 1 || recs[0].ID != "b" || recs[0].Approvals[0].By != "policy" {
		t.Fatalf("%+v %v", recs, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "2026-11.jsonl"))
	// Windows has no Unix permission bits; access there follows the user profile's ACL.
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if recs, _ := l.Read(now.Add(time.Hour)); len(recs) != 0 {
		t.Fatal("since filter ignored")
	}
}

func TestReadMissingDir(t *testing.T) {
	l := &Log{Dir: filepath.Join(t.TempDir(), "none")}
	if recs, err := l.Read(time.Time{}); err != nil || recs != nil {
		t.Fatal(recs, err)
	}
}
