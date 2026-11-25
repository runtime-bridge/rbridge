package accounts

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type spyFS struct {
	mu    sync.Mutex
	reads []string
}

func (s *spyFS) ReadDir(n string) ([]fs.DirEntry, error) { return os.ReadDir(n) }
func (s *spyFS) Stat(n string) (fs.FileInfo, error)      { return os.Stat(n) }
func (s *spyFS) ReadFile(n string) ([]byte, error) {
	s.mu.Lock()
	s.reads = append(s.reads, n)
	s.mu.Unlock()
	return os.ReadFile(n)
}

func write(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestDiscover(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX wrappers")
	}
	home := t.TempDir()
	bin := filepath.Join(home, ".local", "bin")
	// Account folders, with credential files that must never be read.
	for _, d := range []string{".claude", ".claude-pro", ".claude-ar", ".claude-sali", ".claude-solo"} {
		write(t, filepath.Join(home, d, ".credentials.json"), `{"token":"secret"}`, 0o600)
	}
	write(t, filepath.Join(home, ".claude.json"), "{}", 0o600)
	write(t, filepath.Join(home, ".claude-notes.txt"), "not a dir", 0o644)
	write(t, filepath.Join(bin, "claude-pro"), "#!/usr/bin/env bash\nexport CLAUDE_CONFIG_DIR=\"$HOME/.claude-pro\"\nexec claude \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "claude-ar"), "#!/usr/bin/env bash\nexport CLAUDE_CONFIG_DIR=\"$HOME/.claude-ar\"\nsource \"$HOME/.config/claude-ar/env\"\nexec claude \"$@\"\n", 0o755)
	write(t, filepath.Join(home, ".config", "claude-ar", "env"), "export ANTHROPIC_AUTH_TOKEN=secret\n", 0o600)
	write(t, filepath.Join(bin, "claude-work"), "#!/bin/sh\nCLAUDE_CONFIG_DIR=~/work-claude exec claude \"$@\"\n", 0o755)
	write(t, filepath.Join(bin, "claude-noexec"), "#!/bin/sh\nexport CLAUDE_CONFIG_DIR=/x\n", 0o644)
	write(t, filepath.Join(bin, "claude-dynamic"), "#!/bin/sh\nexport CLAUDE_CONFIG_DIR=$(pick)\n", 0o755)
	write(t, filepath.Join(bin, "claude-plain"), "#!/bin/sh\nexec claude --model haiku \"$@\"\n", 0o755)

	spy := &spyFS{}
	got := Discover(Options{FS: spy, Home: home, BinDirs: []string{bin, filepath.Join(home, "missing")}, Wrappers: true, Dirs: true})
	want := map[string]Account{
		"ar":   {Name: "ar", ConfigDir: filepath.Join(home, ".claude-ar"), Launcher: filepath.Join(bin, "claude-ar"), Source: "wrapper"},
		"pro":  {Name: "pro", ConfigDir: filepath.Join(home, ".claude-pro"), Launcher: filepath.Join(bin, "claude-pro"), Source: "wrapper"},
		"sali": {Name: "sali", ConfigDir: filepath.Join(home, ".claude-sali"), Source: "directory"},
		"solo": {Name: "solo", ConfigDir: filepath.Join(home, ".claude-solo"), Source: "directory"},
		"work": {Name: "work", ConfigDir: filepath.Join(home, "work-claude"), Launcher: filepath.Join(bin, "claude-work"), Source: "wrapper"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for _, a := range got {
		if want[a.Name] != a {
			t.Errorf("%s: got %+v want %+v", a.Name, a, want[a.Name])
		}
	}
	for _, r := range spy.reads {
		if !strings.HasPrefix(r, bin) {
			t.Errorf("discovery read a file outside the bin directories: %s", r)
		}
	}
}

func TestDiscoverSwitches(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".claude-x"), 0o755)
	if got := Discover(Options{Home: home}); len(got) != 0 {
		t.Fatalf("both methods off: %+v", got)
	}
	if got := Discover(Options{Home: home, Dirs: true}); len(got) != 1 || got[0].Name != "x" {
		t.Fatalf("dirs on: %+v", got)
	}
}

func TestExpand(t *testing.T) {
	cases := map[string]string{"~/.claude-a": "/h/.claude-a", "$HOME/b": "/h/b", "${HOME}/c": "/h/c", "/abs": "/abs"}
	for in, want := range cases {
		if got, ok := Expand(in, "/h"); !ok || got != filepath.FromSlash(want) {
			t.Errorf("%s -> %s %v", in, got, ok)
		}
	}
	for _, bad := range []string{"relative", "$OTHER/x", "`cmd`"} {
		if _, ok := Expand(bad, "/h"); ok {
			t.Errorf("%s accepted", bad)
		}
	}
}
