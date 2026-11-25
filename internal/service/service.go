// Package service holds rbridge's use cases. The CLI, the Go library and later
// the HTTP and IPC transports all go through it.
package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/governor"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/providers/accounts"
	"github.com/runtime-bridge/rbridge/internal/providers/detect"
	"github.com/runtime-bridge/rbridge/internal/providers/registry"
	"github.com/runtime-bridge/rbridge/internal/redact"
	"github.com/runtime-bridge/rbridge/internal/skills/catalog"
	skillreg "github.com/runtime-bridge/rbridge/internal/skills/registry"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
	"github.com/runtime-bridge/rbridge/internal/store"
	"github.com/runtime-bridge/rbridge/internal/suggest"
)

// Service is the entry point for every interface.
type Service struct {
	paths     config.Paths
	cfg       config.Config
	layers    []config.LayerInfo
	registry  *registry.Registry
	detector  *detect.Detector
	store     store.Store
	getenv    func(string) string
	environ   func() []string
	now       func() time.Time
	slots     chan struct{}
	redact    *redact.Redactor
	catalogs  catalog.Client
	sandboxes sandboxEngines
	gov       *governor.Governor
}

// Governor reports the resource governor, for status displays.
func (s *Service) Governor() *governor.Governor { return s.gov }

// Redactor masks secrets in text rbridge stores or logs.
func (s *Service) Redactor() *redact.Redactor { return s.redact }

// Options wires a Service. Fields left empty get working defaults.
type Options struct {
	Paths    config.Paths
	Config   config.Config
	Layers   []config.LayerInfo
	Store    store.Store
	Detector *detect.Detector
	Getenv   func(string) string
	Environ  func() []string
	Now      func() time.Time
	// AccountsFS is used to discover Claude Code accounts (default the real filesystem).
	AccountsFS accounts.FS
}

// claudeAccounts lists extra Claude Code accounts: configured ones, plus
// discovered ones unless discovery is off. Configured names win.
func claudeAccounts(cfg config.Config, d *detect.Detector, fsys accounts.FS) ([]accounts.Account, error) {
	entry := cfg.Providers.Settings["claude-code"]
	var out []accounts.Account
	names := map[string]bool{}
	dirs := map[string]bool{}
	for _, a := range entry.Accounts {
		dir, ok := accounts.Expand(a.ConfigDir, d.Home)
		if !ok {
			return nil, fmt.Errorf("providers.claude-code.accounts.%s: config_dir must be absolute or start with ~", a.Name)
		}
		out = append(out, accounts.Account{Name: a.Name, ConfigDir: dir, Launcher: a.Launcher, Source: "config"})
		names[a.Name], dirs[dir] = true, true
	}
	if entry.Discovering() && d.Home != "" {
		var bins []string
		for _, b := range filepath.SplitList(d.Getenv("PATH")) {
			if filepath.IsAbs(b) {
				bins = append(bins, b)
			}
		}
		bins = append(bins, filepath.Join(d.Home, ".local", "bin"))
		for _, a := range accounts.Discover(accounts.Options{FS: fsys, Home: d.Home, BinDirs: bins, Wrappers: true, Dirs: true}) {
			if !names[a.Name] && !dirs[a.ConfigDir] {
				out = append(out, a)
			}
		}
	}
	return out, nil
}

// New builds a Service from explicit options.
func New(o Options) (*Service, error) {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Detector == nil {
		return nil, fmt.Errorf("service: detector is required")
	}
	accts, err := claudeAccounts(o.Config, o.Detector, o.AccountsFS)
	if err != nil {
		return nil, err
	}
	reg, err := registry.New(o.Config, accts...)
	if err != nil {
		return nil, err
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Environ == nil {
		o.Environ = os.Environ
	}
	if o.Store == nil {
		st, err := store.NewJSONStore(o.Paths.DataDir)
		if err != nil {
			return nil, err
		}
		o.Store = st
	}
	return &Service{
		paths: o.Paths, cfg: o.Config, layers: o.Layers, registry: reg,
		detector: o.Detector, store: o.Store, getenv: o.Getenv, environ: o.Environ, now: o.Now,
		slots:  make(chan struct{}, max(1, o.Config.Execution.MaxConcurrentSessions)),
		gov:    governor.New(o.Config.Execution.ResourceCeilingPercent, o.Config.Execution.MemoryHeadroomMB),
		redact: redact.New().FromEnviron(o.Environ()),
	}, nil
}

// Environment is what FromEnvironment needs from the running process.
type Environment struct {
	Env        config.Env
	Environ    func() []string
	ProjectDir string
}

// FromEnvironment loads configuration and builds a Service for the current process.
func FromEnvironment(e Environment) (*Service, error) {
	paths, err := config.ResolvePaths(e.Env)
	if err != nil {
		return nil, err
	}
	cfg, layers, err := config.Load(paths, config.LoadOptions{ProjectDir: e.ProjectDir})
	if err != nil {
		return nil, domain.Errorf(domain.ErrInvalidInput, "%v", err)
	}
	det := &detect.Detector{
		FS:           detect.OSFS,
		Run:          process.Run,
		Getenv:       e.Env.Getenv,
		Environ:      e.Environ,
		GOOS:         e.Env.GOOS,
		Home:         e.Env.HomeDir,
		EnvAllowlist: cfg.Policy.EnvAllowlist,
		Timeout:      cfg.Execution.DetectTimeout,
	}
	st, err := store.NewJSONStore(paths.DataDir)
	if err != nil {
		return nil, err
	}
	return New(Options{Paths: paths, Config: cfg, Layers: layers, Store: st, Detector: det, Getenv: e.Env.Getenv, Environ: e.Environ})
}

// Skills returns the skill library.
func (s *Service) Skills() *skillreg.Registry {
	return &skillreg.Registry{
		Dir: filepath.Join(s.paths.DataDir, "skills"), Store: s.store, Limits: spec.DefaultLimits,
		Now: func() time.Time { return s.now().UTC() }, RequirePinned: s.cfg.Skills.GitRequirePinnedRevision,
	}
}

// Paths returns the resolved directories.
func (s *Service) Paths() config.Paths { return s.paths }

// Config returns the effective configuration.
func (s *Service) Config() config.Config { return s.cfg }

// Layers returns the configuration layers that were considered.
func (s *Service) Layers() []config.LayerInfo { return s.layers }

// SkillLocations are a provider's native skill directories, resolved for this machine.
type SkillLocations struct {
	Project    []string `json:"project"`
	User       []string `json:"user"`
	SessionDir string   `json:"session_dir,omitempty"`
}

// ProviderStatus is everything rbridge knows about one provider.
type ProviderStatus struct {
	ID           string               `json:"id"`
	DisplayName  string               `json:"display_name"`
	Class        domain.ProviderClass `json:"class"`
	BuiltIn      bool                 `json:"built_in"`
	Runtime      domain.RuntimeInfo   `json:"runtime"`
	Capabilities domain.CapabilitySet `json:"capabilities"`
	Skills       SkillLocations       `json:"skills"`
	Limits       []string             `json:"limits,omitempty"`
	Notes        []string             `json:"notes,omitempty"`
	Cached       bool                 `json:"cached"`
}

// Providers detects every provider, using the cache unless refresh is set.
func (s *Service) Providers(ctx context.Context, refresh bool) ([]ProviderStatus, error) {
	var out []ProviderStatus
	for _, m := range s.registry.All() {
		st, err := s.status(ctx, m, refresh)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// noProvider is the error for an unknown provider id: the closest id, if
// any, and the ids there are.
func (s *Service) noProvider(id string) error {
	var ids []string
	for _, m := range s.registry.All() {
		ids = append(ids, m.ID)
	}
	return domain.Errorf(domain.ErrProviderNotFound, "no provider %q%s (providers: %s; rbridge providers shows which are installed)", id, suggest.DidYouMean(id, ids), strings.Join(ids, ", "))
}

// Provider detects one provider.
func (s *Service) Provider(ctx context.Context, id string, refresh bool) (ProviderStatus, error) {
	m, ok := s.registry.Get(id)
	if !ok {
		return ProviderStatus{}, s.noProvider(id)
	}
	return s.status(ctx, m, refresh)
}

// Lookup returns a provider as a domain.Provider.
func (s *Service) Lookup(id string) (domain.Provider, error) {
	m, ok := s.registry.Get(id)
	if !ok {
		return nil, s.noProvider(id)
	}
	return &provider{svc: s, m: m}, nil
}

type provider struct {
	svc *Service
	m   registry.Manifest
}

func (p *provider) ID() string { return p.m.ID }

func (p *provider) Detect(ctx context.Context) (domain.RuntimeInfo, error) {
	st, err := p.svc.status(ctx, p.m, true)
	return st.Runtime, err
}

func (p *provider) Capabilities(context.Context) (domain.CapabilitySet, error) {
	return p.m.Capabilities.Merge(nil), nil
}

const detectCollection = "detect"

type cachedDetection struct {
	Fingerprint string             `json:"fingerprint"`
	Info        domain.RuntimeInfo `json:"info"`
}

func (s *Service) status(ctx context.Context, m registry.Manifest, refresh bool) (ProviderStatus, error) {
	entry := s.cfg.Providers.Settings[m.ID]
	fp := s.fingerprint(m, entry)
	info, cached := domain.RuntimeInfo{}, false

	if !refresh && s.cfg.Execution.DetectCacheTTL > 0 {
		var c cachedDetection
		ok, err := s.store.Get(ctx, detectCollection, m.ID, &c)
		if err == nil && ok && c.Fingerprint == fp && s.now().Sub(c.Info.DetectedAt) < s.cfg.Execution.DetectCacheTTL {
			info, cached = c.Info, true
		}
	}
	if !cached {
		info = s.detector.Detect(ctx, m, entry)
		if err := ctx.Err(); err != nil {
			// A cancelled detection says nothing about the runtime; do not report or cache it.
			return ProviderStatus{}, &domain.Error{Code: domain.ErrCancelled, Message: "detection cancelled", Provider: m.ID, Cause: err}
		}
		if s.cfg.Execution.DetectCacheTTL > 0 {
			// A cache write failure only costs a re-detection next time.
			_ = s.store.Put(ctx, detectCollection, m.ID, cachedDetection{Fingerprint: fp, Info: info})
		}
	}

	s.checkExecutable(ctx, m.ID, &info)
	if info.AuthMode == domain.AuthUnknown {
		var seen observedAuth
		if ok, _ := s.store.Get(ctx, authCollection, m.ID, &seen); ok && seen.Mode != "" {
			info.AuthMode = seen.Mode
			info.Notes = append(info.Notes, "sign-in mode "+string(seen.Mode)+" seen in the session at "+seen.At.Local().Format("2006-01-02 15:04"))
		}
	}
	notes := append([]string(nil), m.Notes...)
	notes = append(notes, info.Notes...)
	return ProviderStatus{
		ID:           m.ID,
		DisplayName:  m.DisplayName,
		Class:        m.Class,
		BuiltIn:      m.BuiltIn,
		Runtime:      info,
		Capabilities: m.Capabilities.Merge(nil),
		Skills:       s.skillLocations(m),
		Limits:       m.Limits,
		Notes:        notes,
		Cached:       cached,
	}, nil
}

// fingerprint changes whenever something that affects detection changes.
func (s *Service) fingerprint(m registry.Manifest, e config.ProviderEntry) string {
	b, _ := json.Marshal(struct {
		Detect registry.DetectSpec
		Entry  config.ProviderEntry
		Path   string
		Auth   []bool
	}{m.Detect, e, s.getenv("PATH"), s.authEnvPresence(m)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

func (s *Service) authEnvPresence(m registry.Manifest) []bool {
	out := make([]bool, len(m.AuthEnv))
	for i, n := range m.AuthEnv {
		out[i] = s.getenv(n) != ""
	}
	return out
}

func (s *Service) skillLocations(m registry.Manifest) SkillLocations {
	loc := SkillLocations{Project: m.Skills.Project, SessionDir: m.Skills.SessionDir}
	seen := map[string]bool{}
	for _, u := range m.Skills.User {
		var p string
		for alt := range strings.SplitSeq(u, "|") {
			if p = s.detector.Expand(alt); p != "" {
				break
			}
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		loc.User = append(loc.User, p)
	}
	return loc
}

// ensureDir creates dir with owner-only permissions and checks it is writable.
func ensureDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".rbridge-write-check-*")
	if err != nil {
		return err
	}
	name := f.Name()
	f.Close()
	return os.Remove(filepath.Clean(name))
}

const seenCollection = "seen"

// seenExecutable is the executable rbridge last accepted for a provider.
type seenExecutable struct {
	Path     string    `json:"path"`
	Resolved string    `json:"resolved"`
	Version  string    `json:"version"`
	Size     int64     `json:"size"`
	At       time.Time `json:"at"`
}

func (s *Service) currentExecutable(info domain.RuntimeInfo) (seenExecutable, bool) {
	if info.Executable == "" || (info.Health != domain.HealthAvailable && info.Health != domain.HealthNeedsAPIKey) {
		return seenExecutable{}, false
	}
	cur := seenExecutable{Path: info.Executable, Resolved: info.Executable, Version: info.Version, At: s.now()}
	if r, err := filepath.EvalSymlinks(info.Executable); err == nil {
		cur.Resolved = r
	}
	if fi, err := os.Stat(cur.Resolved); err == nil {
		cur.Size = fi.Size()
	}
	return cur, true
}

// checkExecutable compares the detected executable with the accepted one.
// A different path, or the same version with different contents, is flagged
// until the user accepts it; an upgrade in place is accepted with a note.
func (s *Service) checkExecutable(ctx context.Context, id string, info *domain.RuntimeInfo) {
	cur, ok := s.currentExecutable(*info)
	if !ok {
		return
	}
	var prev seenExecutable
	found, err := s.store.Get(ctx, seenCollection, id, &prev)
	if err != nil {
		return
	}
	switch {
	case !found:
		_ = s.store.Put(ctx, seenCollection, id, cur)
	case prev.Path != cur.Path:
		info.Changed = fmt.Sprintf("executable is now %s, was %s", cur.Path, prev.Path)
	case prev.Version == cur.Version && prev.Size != 0 && cur.Size != 0 && prev.Size != cur.Size:
		info.Changed = fmt.Sprintf("%s changed size (%d to %d bytes) without a version change", cur.Path, prev.Size, cur.Size)
	case prev.Version != cur.Version || prev.Resolved != cur.Resolved:
		info.Notes = append(info.Notes, fmt.Sprintf("updated from %s to %s", orDash(prev.Version), orDash(cur.Version)))
		_ = s.store.Put(ctx, seenCollection, id, cur)
	}
	if info.Changed != "" {
		info.Notes = append(info.Notes, "not the executable rbridge last used: "+info.Changed+"; run rbridge providers accept "+id+" if you expected this")
	}
}

// AcceptExecutable records the provider's current executable as expected.
func (s *Service) AcceptExecutable(ctx context.Context, id string) (domain.RuntimeInfo, error) {
	m, ok := s.registry.Get(id)
	if !ok {
		return domain.RuntimeInfo{}, s.noProvider(id)
	}
	info := s.detector.Detect(ctx, m, s.cfg.Providers.Settings[id])
	cur, ok := s.currentExecutable(info)
	if !ok {
		return info, domain.Errorf(domain.ErrProviderUnavailable, "%s has no runnable executable to accept", id)
	}
	return info, s.store.Put(ctx, seenCollection, id, cur)
}
