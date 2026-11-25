//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/process"
	"github.com/rbridge/rbridge/internal/providers/detect"
	"github.com/rbridge/rbridge/internal/store"
)

type harness struct {
	home  string
	bin   string
	env   map[string]string
	now   time.Time
	runs  int
	store *store.MemStore
}

// newRunHarness0 is newHarness under another name, for tests that read better that way.
func newRunHarness0(t *testing.T) *harness { return newHarness(t) }

func newHarness(t *testing.T) *harness {
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return &harness{home: home, bin: bin, env: map[string]string{"PATH": bin}, now: time.Unix(1_700_000_000, 0), store: store.NewMemStore()}
}

func (h *harness) exe(t *testing.T, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) service(t *testing.T, cfg config.Config) *Service {
	t.Helper()
	paths, err := config.ResolvePaths(config.Env{GOOS: "linux", Getenv: func(k string) string {
		if k == "RBRIDGE_HOME" {
			return filepath.Join(h.home, "rb")
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(k string) string { return h.env[k] }
	det := &detect.Detector{
		FS: detect.OSFS,
		Run: func(ctx context.Context, s process.Spec) (process.Output, error) {
			h.runs++
			return process.Run(ctx, s)
		},
		Getenv:       getenv,
		Environ:      func() []string { return []string{"PATH=" + h.env["PATH"] + ":/usr/bin:/bin", "HOME=" + h.home} },
		GOOS:         "linux",
		Home:         h.home,
		EnvAllowlist: []string{"PATH"},
		Timeout:      2 * time.Second,
		Now:          func() time.Time { return h.now },
	}
	svc, err := New(Options{Paths: paths, Config: cfg, Store: h.store, Detector: det, Getenv: getenv, Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestProvidersUseCache(t *testing.T) {
	h := newHarness(t)
	h.exe(t, "claude", `echo "2.1.331 (Claude Code)"`)
	h.exe(t, "opencode", `echo 1.19.6`)
	svc := h.service(t, config.Defaults())
	ctx := context.Background()

	list, err := svc.Providers(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 5 || list[0].ID != "claude-code" || list[0].Runtime.Health != domain.HealthAvailable || list[0].Cached {
		t.Fatalf("first run: %+v", list)
	}
	first := h.runs

	list, _ = svc.Providers(ctx, false)
	if h.runs != first || !list[0].Cached {
		t.Fatal("second call should come from the cache")
	}

	h.env["PATH"] = h.bin + ":/nonexistent"
	list, _ = svc.Providers(ctx, false)
	if h.runs == first || list[0].Cached {
		t.Fatal("a PATH change must invalidate the cache")
	}

	runs := h.runs
	h.now = h.now.Add(11 * time.Minute)
	svc.Providers(ctx, false)
	if h.runs == runs {
		t.Fatal("an expired entry must be detected again")
	}

	runs = h.runs
	svc.Providers(ctx, true)
	if h.runs == runs {
		t.Fatal("refresh must bypass the cache")
	}
}

func TestCacheInvalidatedByAPIKeyPresence(t *testing.T) {
	h := newHarness(t)
	h.exe(t, "claude", `echo 2.1.331`)
	svc := h.service(t, config.Defaults())
	ctx := context.Background()
	p, _ := svc.Provider(ctx, "claude-code", false)
	if p.Runtime.AuthMode != domain.AuthUnknown {
		t.Fatalf("auth = %s", p.Runtime.AuthMode)
	}
	h.env["ANTHROPIC_API_KEY"] = "x"
	p, _ = svc.Provider(ctx, "claude-code", false)
	if p.Cached || p.Runtime.AuthMode != domain.AuthAPIKey {
		t.Fatalf("auth change not picked up: %+v", p.Runtime)
	}
}

func TestProviderNotFound(t *testing.T) {
	svc := newHarness(t).service(t, config.Defaults())
	_, err := svc.Provider(context.Background(), "nope", false)
	if domain.CodeOf(err) != domain.ErrProviderNotFound {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Lookup("nope"); domain.CodeOf(err) != domain.ErrProviderNotFound {
		t.Fatalf("lookup err = %v", err)
	}
}

func TestLookupImplementsProvider(t *testing.T) {
	h := newHarness(t)
	h.exe(t, "opencode", `echo 1.19.6`)
	svc := h.service(t, config.Defaults())
	p, err := svc.Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	info, err := p.Detect(context.Background())
	if err != nil || info.Version != "1.19.6" {
		t.Fatalf("detect: %+v %v", info, err)
	}
	caps, _ := p.Capabilities(context.Background())
	if caps.Get(domain.CapSessionsLoad).Supported != domain.SupportYes {
		t.Fatal("opencode capabilities missing")
	}
}

func TestClaudeAccountsAreSeparateProviders(t *testing.T) {
	h := newRunHarness0(t)
	// A wrapper for one account and a bare directory for another.
	os.MkdirAll(filepath.Join(h.home, ".claude-alt"), 0o755)
	os.MkdirAll(filepath.Join(h.home, ".claude-work"), 0o755)
	h.exe(t, "claude", `echo "2.1.331 (Claude Code)"`)
	h.exe(t, "claude-work", "export CLAUDE_CONFIG_DIR=\"$HOME/.claude-work\"\nexec claude \"$@\"")
	h.env["CLAUDE_CONFIG_DIR"] = filepath.Join(h.home, ".claude-alt")
	svc := h.service(t, config.Defaults())
	ctx := context.Background()

	def, _ := svc.Provider(ctx, "claude-code", false)
	if len(def.Skills.User) != 1 || def.Skills.User[0] != filepath.Join(h.home, ".claude", "skills") {
		t.Fatalf("claude-code must stay on ~/.claude whatever the shell sets: %v", def.Skills.User)
	}
	alt, err := svc.Provider(ctx, "claude-code-alt", false)
	if err != nil || alt.Skills.User[0] != filepath.Join(h.home, ".claude-alt", "skills") || alt.Runtime.Health != domain.HealthAvailable {
		t.Fatalf("directory account: %+v %v", alt, err)
	}
	work, err := svc.Provider(ctx, "claude-code-work", false)
	if err != nil || work.Runtime.Executable != filepath.Join(h.bin, "claude-work") || work.Runtime.Version != "2.1.331" {
		t.Fatalf("wrapper account: %+v %v", work.Runtime, err)
	}
	r := svc.Doctor(ctx)
	found := false
	for _, c := range r.Checks {
		if c.Name == "claude accounts" && strings.Contains(c.Detail, "claude-code-alt") {
			found = true
		}
	}
	if !found {
		t.Fatalf("doctor should say which provider the shell's account is: %+v", r.Checks)
	}

	off := false
	cfg := config.Defaults()
	cfg.Providers.Settings["claude-code"] = config.ProviderEntry{DiscoverAccounts: &off, Accounts: []config.AccountConfig{{Name: "team", ConfigDir: "~/.team-claude"}}}
	svc = h.service(t, cfg)
	if _, err := svc.Provider(ctx, "claude-code-alt", false); domain.CodeOf(err) != domain.ErrProviderNotFound {
		t.Fatal("discovery should be off")
	}
	team, err := svc.Provider(ctx, "claude-code-team", false)
	if err != nil || team.Skills.User[0] != filepath.Join(h.home, ".team-claude", "skills") {
		t.Fatalf("configured account: %+v %v", team, err)
	}
}

func TestDoctor(t *testing.T) {
	h := newHarness(t)
	svc := h.service(t, config.Defaults())
	r := svc.Doctor(context.Background())
	if !r.Failed() {
		t.Fatal("doctor must fail when no runtime is available")
	}
	h.exe(t, "opencode", `echo 1.19.6`)
	h.exe(t, "claude", `exit 1`)
	r = svc.Doctor(context.Background())
	byName := map[string]Check{}
	for _, c := range r.Checks {
		byName[c.Name] = c
	}
	if byName["provider opencode"].Status != CheckOK {
		t.Errorf("opencode: %+v", byName["provider opencode"])
	}
	if c := byName["provider claude-code"]; c.Status != CheckFail || c.Fix == "" {
		t.Errorf("claude-code: %+v", c)
	}
	if byName["data directory"].Status != CheckOK {
		t.Errorf("data dir: %+v", byName["data directory"])
	}
}

func TestDoctorReportsDisabledAndMissing(t *testing.T) {
	h := newHarness(t)
	h.exe(t, "opencode", `echo 1.19.6`)
	cfg := config.Defaults()
	off := false
	cfg.Providers.Settings["claude-code"] = config.ProviderEntry{Enabled: &off}
	r := h.service(t, cfg).Doctor(context.Background())
	if r.Failed() {
		t.Fatalf("unexpected failure: %+v", r)
	}
	for _, c := range r.Checks {
		if c.Name == "provider claude-code" && c.Detail != "disabled in configuration" {
			t.Errorf("claude-code: %+v", c)
		}
	}
}
