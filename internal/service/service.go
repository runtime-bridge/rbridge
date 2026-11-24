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

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/process"
	"github.com/rbridge/rbridge/internal/providers/detect"
	"github.com/rbridge/rbridge/internal/providers/registry"
	"github.com/rbridge/rbridge/internal/store"
)

// Service is the entry point for every interface.
type Service struct {
	paths    config.Paths
	cfg      config.Config
	layers   []config.LayerInfo
	registry *registry.Registry
	detector *detect.Detector
	store    store.Store
	getenv   func(string) string
	now      func() time.Time
}

// Options wires a Service. Fields left empty get working defaults.
type Options struct {
	Paths    config.Paths
	Config   config.Config
	Layers   []config.LayerInfo
	Store    store.Store
	Detector *detect.Detector
	Getenv   func(string) string
	Now      func() time.Time
}

// New builds a Service from explicit options.
func New(o Options) (*Service, error) {
	reg, err := registry.New(o.Config)
	if err != nil {
		return nil, err
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Store == nil {
		st, err := store.NewJSONStore(o.Paths.CacheDir)
		if err != nil {
			return nil, err
		}
		o.Store = st
	}
	if o.Detector == nil {
		return nil, fmt.Errorf("service: detector is required")
	}
	return &Service{
		paths: o.Paths, cfg: o.Config, layers: o.Layers, registry: reg,
		detector: o.Detector, store: o.Store, getenv: o.Getenv, now: o.Now,
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
	return New(Options{Paths: paths, Config: cfg, Layers: layers, Detector: det, Getenv: e.Env.Getenv})
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

// Provider detects one provider.
func (s *Service) Provider(ctx context.Context, id string, refresh bool) (ProviderStatus, error) {
	m, ok := s.registry.Get(id)
	if !ok {
		return ProviderStatus{}, domain.Errorf(domain.ErrProviderNotFound, "no provider %q", id)
	}
	return s.status(ctx, m, refresh)
}

// Lookup returns a provider as a domain.Provider.
func (s *Service) Lookup(id string) (domain.Provider, error) {
	m, ok := s.registry.Get(id)
	if !ok {
		return nil, domain.Errorf(domain.ErrProviderNotFound, "no provider %q", id)
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
		if s.cfg.Execution.DetectCacheTTL > 0 {
			// A cache write failure only costs a re-detection next time.
			_ = s.store.Put(ctx, detectCollection, m.ID, cachedDetection{Fingerprint: fp, Info: info})
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
