package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/policy"
	"github.com/runtime-bridge/rbridge/internal/providers/registry"
	"github.com/runtime-bridge/rbridge/internal/sandbox"
)

type sandboxEngines struct {
	mu    sync.Mutex
	found map[string]sandboxFound
}

type sandboxFound struct {
	engine sandbox.Engine
	notes  []string
}

// engine finds the sandbox engine once per process for each choice.
func (s *Service) engine(ctx context.Context) (sandbox.Engine, []string) {
	choice := s.cfg.Sandbox.Engine
	s.sandboxes.mu.Lock()
	defer s.sandboxes.mu.Unlock()
	if f, ok := s.sandboxes.found[choice]; ok {
		return f.engine, f.notes
	}
	e, notes := sandbox.Detect(ctx, choice)
	if s.sandboxes.found == nil {
		s.sandboxes.found = map[string]sandboxFound{}
	}
	s.sandboxes.found[choice] = sandboxFound{e, notes}
	return e, notes
}

// sandboxMode is the task's choice, then the custom profile's, then the
// configured default.
func (s *Service) sandboxMode(t domain.Task, prof policy.Profile) (sandbox.Mode, error) {
	mode := s.cfg.Sandbox.Mode
	if prof.Name == "custom" && s.cfg.Policy.Custom.Sandbox != "" {
		mode = s.cfg.Policy.Custom.Sandbox
	}
	if t.Sandbox != "" {
		mode = t.Sandbox
	}
	m, err := sandbox.ParseMode(mode)
	if err != nil {
		return sandbox.Off, domain.Errorf(domain.ErrInvalidInput, "%v", err)
	}
	return m, nil
}

// sandboxFor starts the session's sandbox when one is asked for. A sandbox
// that is asked for but cannot be had fails the task; it never runs
// unsandboxed instead.
func (s *Service) sandboxFor(ctx context.Context, m registry.Manifest, st ProviderStatus, t domain.Task, prof policy.Profile, id, scratch string) (domain.Launcher, policy.Sandbox, func(), error) {
	mode, err := s.sandboxMode(t, prof)
	if err != nil || mode == sandbox.Off || m.Integration == domain.IntegrationModelServer {
		return nil, policy.Sandbox{}, nil, err
	}
	engine, notes := s.engine(ctx)
	if engine == nil {
		return nil, policy.Sandbox{}, nil, domain.Errorf(domain.ErrPolicyUnenforceable,
			"sandbox %s was asked for, but no sandbox engine works here (%s)", mode, strings.Join(notes, "; "))
	}
	spec := sandbox.Spec{Mode: mode, Workspace: t.Workspace, Scratch: scratch, Name: id}
	for _, d := range append(append([]string(nil), m.StateDirs...), s.cfg.Sandbox.Writable...) {
		if p := s.detector.Expand(d); p != "" {
			spec.Writable = append(spec.Writable, p)
		}
	}
	if exe := st.Runtime.Executable; exe != "" {
		spec.Visible = append(spec.Visible, filepath.Dir(exe))
		if real, err := filepath.EvalSymlinks(exe); err == nil {
			spec.Visible = append(spec.Visible, filepath.Dir(real))
		}
	}
	if home := s.getenv("HOME"); home != "" {
		spec.Visible = append(spec.Visible, home)
	}
	// Delivered skills link into the library.
	spec.Visible = append(spec.Visible, s.paths.DataDir)
	if _, err := os.Stat(s.paths.DataDir); err != nil {
		spec.Visible = spec.Visible[:len(spec.Visible)-1]
	}
	l, cleanup, err := engine.Start(ctx, spec)
	if err != nil {
		return nil, policy.Sandbox{}, nil, domain.Errorf(domain.ErrPolicyUnenforceable, "sandbox %s could not start: %v", engine.Name(), err)
	}
	return l, policy.Sandbox{Engine: engine.Name(), Mode: string(mode)}, cleanup, nil
}
