//go:build unix

package detect

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/providers/registry"
)

// spyFS records every path detection touches.
type spyFS struct {
	mu    sync.Mutex
	paths []string
}

func (s *spyFS) record(p string) {
	s.mu.Lock()
	s.paths = append(s.paths, p)
	s.mu.Unlock()
}

func (s *spyFS) Stat(p string) (fs.FileInfo, error)    { s.record(p); return os.Stat(p) }
func (s *spyFS) EvalSymlinks(p string) (string, error) { s.record(p); return filepath.EvalSymlinks(p) }

func fakeExe(t *testing.T, dir, name, script string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

type fixture struct {
	home string
	bin  string
	env  map[string]string
	fs   *spyFS
}

func newFixture(t *testing.T) *fixture {
	home := t.TempDir()
	bin := filepath.Join(home, "bin-on-path")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return &fixture{home: home, bin: bin, env: map[string]string{"PATH": bin}, fs: &spyFS{}}
}

func (f *fixture) detector() *Detector {
	return &Detector{
		FS:           f.fs,
		Run:          process.Run,
		Getenv:       func(k string) string { return f.env[k] },
		Environ:      func() []string { return []string{"PATH=/usr/bin:/bin", "SECRET=nope"} },
		GOOS:         "linux",
		Home:         f.home,
		EnvAllowlist: []string{"PATH"},
		Timeout:      2 * time.Second,
		Now:          func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

func manifest(exe string, known ...string) registry.Manifest {
	m := registry.OpenCode()
	m.Detect.Executables = []string{exe}
	m.Detect.KnownDirs = known
	return m
}

func TestDetectAvailable(t *testing.T) {
	f := newFixture(t)
	fakeExe(t, f.bin, "fake-agent", `echo "fake-agent 1.19.6"`)
	info := f.detector().Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{})
	if info.Health != domain.HealthAvailable || info.Version != "1.19.6" || info.Executable != filepath.Join(f.bin, "fake-agent") {
		t.Fatalf("unexpected info %+v", info)
	}
	if info.AuthMode != domain.AuthUnknown {
		t.Errorf("auth mode %s", info.AuthMode)
	}
}

func TestDetectMissing(t *testing.T) {
	f := newFixture(t)
	info := f.detector().Detect(context.Background(), manifest("not-installed"), config.ProviderEntry{})
	if info.Health != domain.HealthMissing || len(info.Installations) != 0 {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestDetectDisabled(t *testing.T) {
	f := newFixture(t)
	fakeExe(t, f.bin, "fake-agent", `echo 1.0.0`)
	off := false
	info := f.detector().Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{Enabled: &off})
	if info.Health != domain.HealthDisabled || len(info.Installations) != 0 {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestDetectUnhealthy(t *testing.T) {
	cases := map[string]string{
		"exit code":  `echo broken >&2; exit 4`,
		"no version": `echo hello`,
		"hangs":      `sleep 30`,
	}
	for name, script := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			fakeExe(t, f.bin, "fake-agent", script)
			d := f.detector()
			d.Timeout = 300 * time.Millisecond
			info := d.Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{})
			if info.Health != domain.HealthUnhealthy || info.Installations[0].Error == "" {
				t.Fatalf("unexpected info %+v", info)
			}
		})
	}
}

func TestDetectNotExecutable(t *testing.T) {
	f := newFixture(t)
	p := fakeExe(t, f.bin, "fake-agent", `echo 1.0.0`)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	info := f.detector().Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{})
	if info.Health != domain.HealthUnhealthy {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestDetectSeveralInstallsAndDedupe(t *testing.T) {
	f := newFixture(t)
	first := fakeExe(t, f.bin, "fake-agent", `echo 2.0.0`)
	other := filepath.Join(f.home, "other")
	fakeExe(t, other, "fake-agent", `echo 1.5.0`)
	link := filepath.Join(f.home, "linkdir")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(first, filepath.Join(link, "fake-agent")); err != nil {
		t.Fatal(err)
	}
	m := manifest("fake-agent", "~/other", "~/linkdir")
	info := f.detector().Detect(context.Background(), m, config.ProviderEntry{})
	if len(info.Installations) != 2 {
		t.Fatalf("want 2 installations (symlink deduped), got %+v", info.Installations)
	}
	if info.Executable != first || info.Version != "2.0.0" {
		t.Fatalf("PATH entry should win: %+v", info)
	}
	if !strings.Contains(strings.Join(info.Notes, " "), "pin one") {
		t.Errorf("expected a pin note, got %v", info.Notes)
	}
}

func TestDetectPinned(t *testing.T) {
	f := newFixture(t)
	fakeExe(t, f.bin, "fake-agent", `echo 2.0.0`)
	pinned := fakeExe(t, filepath.Join(f.home, "pinned"), "fake-agent", `echo 0.9.1`)
	info := f.detector().Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{Executable: pinned})
	if !info.Pinned || info.Executable != pinned || info.Version != "0.9.1" || len(info.Installations) != 1 {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestVersionCommandGetsFilteredEnv(t *testing.T) {
	f := newFixture(t)
	// The fake reports a different version when the secret leaks through.
	fakeExe(t, f.bin, "fake-agent", `if [ -n "$SECRET" ]; then echo 9.9.9; else echo 1.2.3; fi`)
	info := f.detector().Detect(context.Background(), manifest("fake-agent"), config.ProviderEntry{})
	if info.Version != "1.2.3" {
		t.Fatalf("parent environment leaked into the version command: version %q", info.Version)
	}
}

func TestAuthModeFromEnvNamesOnly(t *testing.T) {
	f := newFixture(t)
	fakeExe(t, f.bin, "claude", `echo "2.1.331 (Claude Code)"`)
	m := registry.ClaudeCode()
	m.Detect.KnownDirs = nil
	d := f.detector()
	info := d.Detect(context.Background(), m, config.ProviderEntry{})
	if info.AuthMode != domain.AuthUnknown || info.Version != "2.1.331" {
		t.Fatalf("unexpected %+v", info)
	}
	f.env["ANTHROPIC_API_KEY"] = "set"
	info = d.Detect(context.Background(), m, config.ProviderEntry{})
	if info.AuthMode != domain.AuthAPIKey {
		t.Fatalf("auth mode %s", info.AuthMode)
	}
}

// TestNeverTouchesCredentialFiles plants credential files in a fake home and
// checks that detection of every built-in provider leaves them alone.
func TestNeverTouchesCredentialFiles(t *testing.T) {
	f := newFixture(t)
	f.env["CLAUDE_CONFIG_DIR"] = filepath.Join(f.home, ".claude-alt")
	f.env["APPDATA"] = filepath.Join(f.home, "appdata")
	secrets := []string{
		".claude/.credentials.json",
		".claude.json",
		".claude-alt/.credentials.json",
		".config/opencode/opencode.json",
		".local/share/opencode/auth.json",
		".codex/auth.json",
		".gemini/oauth_creds.json",
	}
	for _, s := range secrets {
		p := filepath.Join(f.home, s)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(`{"token":"secret"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fakeExe(t, filepath.Join(f.home, ".local", "bin"), "claude", `echo 2.1.331`)
	fakeExe(t, filepath.Join(f.home, ".opencode", "bin"), "opencode", `echo 1.19.6`)
	for _, n := range []string{"codex-acp", "gemini", "claude-agent-acp"} {
		fakeExe(t, filepath.Join(f.home, ".local", "bin"), n, `echo 1.0.0`)
	}

	d := f.detector()
	for _, m := range registry.BuiltIns() {
		if m.Integration == domain.IntegrationModelServer {
			continue // reached over HTTP; detection reads no files
		}
		info := d.Detect(context.Background(), m, config.ProviderEntry{})
		if info.Health != domain.HealthAvailable && info.Health != domain.HealthNeedsAPIKey {
			t.Fatalf("%s: %+v", m.ID, info)
		}
	}
	executables := map[string]bool{"claude": true, "opencode": true, "codex-acp": true, "gemini": true, "claude-agent-acp": true}
	for _, p := range f.fs.paths {
		for _, s := range secrets {
			if strings.HasPrefix(p, filepath.Join(f.home, s)) {
				t.Errorf("detection touched credential path %s", p)
			}
		}
		if !executables[filepath.Base(p)] {
			t.Errorf("detection touched a non-executable path %s", p)
		}
	}
}

func TestExpand(t *testing.T) {
	d := &Detector{Home: "/h", Getenv: func(k string) string {
		return map[string]string{"CLAUDE_CONFIG_DIR": "/c", "REL": "rel"}[k]
	}}
	cases := map[string]string{
		"~/.local/bin":              "/h/.local/bin",
		"~":                         "/h",
		"$CLAUDE_CONFIG_DIR/skills": "/c/skills",
		"$UNSET/skills":             "",
		"$REL/x":                    "",
		"/usr/local/bin":            "/usr/local/bin",
		"relative/dir":              "",
	}
	for in, want := range cases {
		if got := d.Expand(in); got != want {
			t.Errorf("Expand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWindowsNames(t *testing.T) {
	d := &Detector{GOOS: "windows", Getenv: func(k string) string {
		if k == "PATHEXT" {
			return ".EXE;.CMD"
		}
		return ""
	}}
	got := d.names("claude")
	if len(got) != 2 || got[0] != "claude.exe" || got[1] != "claude.cmd" {
		t.Fatalf("names = %v", got)
	}
	if got := d.names("tool.exe"); len(got) != 1 {
		t.Fatalf("names with extension = %v", got)
	}
}

func TestAPIKeyGate(t *testing.T) {
	f := newFixture(t)
	fakeExe(t, f.bin, "gemini", `echo 0.40.0`)
	fakeExe(t, f.bin, "claude-agent-acp", `echo 0.9.0`)
	d := f.detector()
	g := registry.GeminiCLI()
	g.Detect.KnownDirs = nil
	c := registry.ClaudeACP()
	c.Detect.KnownDirs = nil
	if info := d.Detect(context.Background(), g, config.ProviderEntry{}); info.Health != domain.HealthNeedsAPIKey {
		t.Fatalf("gemini without key: %+v", info)
	}
	if info := d.Detect(context.Background(), g, config.ProviderEntry{AllowAccountLogin: true}); info.Health != domain.HealthAvailable || info.AuthMode != domain.AuthSubscription {
		t.Fatalf("gemini with account login allowed: %+v", info)
	}
	if info := d.Detect(context.Background(), c, config.ProviderEntry{AllowAccountLogin: true}); info.Health != domain.HealthNeedsAPIKey {
		t.Fatalf("claude-acp must ignore allow_account_login: %+v", info)
	}
	f.env["ANTHROPIC_API_KEY"] = "x"
	f.env["GEMINI_API_KEY"] = "x"
	for _, m := range []registry.Manifest{g, c} {
		if info := d.Detect(context.Background(), m, config.ProviderEntry{}); info.Health != domain.HealthAvailable || info.AuthMode != domain.AuthAPIKey {
			t.Fatalf("%s with key: %+v", m.ID, info)
		}
	}
}

func TestServerURL(t *testing.T) {
	m := registry.Ollama()
	env := map[string]string{}
	d := &Detector{Getenv: func(k string) string { return env[k] }}
	check := func(entry config.ProviderEntry, want string, ok bool) {
		t.Helper()
		got, err := d.ServerURL(m, entry)
		if (err == nil) != ok || got != want {
			t.Errorf("got %q %v, want %q", got, err, want)
		}
	}
	check(config.ProviderEntry{}, "http://127.0.0.1:11434", true)
	env["OLLAMA_HOST"] = "0.0.0.0:9999"
	check(config.ProviderEntry{}, "http://127.0.0.1:9999", true)
	check(config.ProviderEntry{URL: "http://localhost:7000"}, "http://localhost:7000", true)
	env["OLLAMA_HOST"] = "http://10.0.0.8:11434"
	check(config.ProviderEntry{}, "", false)
}
