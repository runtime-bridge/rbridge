//go:build unix

package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
)

func TestModelKeywordsForClaudeCode(t *testing.T) {
	for _, c := range []struct {
		model, settings, want string
	}{
		{"latest", "", "--model\nopus"},
		{"auto", "", "--model\ndefault"},
		{"fast", "fast=haiku", "--model\nhaiku"}, // a name of your own from providers.<id>.models
		{"latest", "latest=sonnet", "--model\nsonnet"},
		{"claude-sonnet-5-5", "", "--model\nclaude-sonnet-5-5"},
	} {
		h := newRunHarness(t)
		argsFile := filepath.Join(h.home, "args")
		h.env[fakeclaude.EnvArgsFile] = argsFile
		h.cfg.Policy.EnvAllowlist = append(h.cfg.Policy.EnvAllowlist, fakeclaude.EnvArgsFile)
		if c.settings != "" {
			k, v, _ := strings.Cut(c.settings, "=")
			h.cfg.Providers.Settings["claude-code"] = config.ProviderEntry{Models: map[string]string{k: v}}
		}
		res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("x"), Provider: "claude-code"}, RunOptions{Model: c.model})
		if err != nil {
			t.Fatalf("%s: %v", c.model, err)
		}
		b, _ := os.ReadFile(argsFile)
		if !strings.Contains(string(b), c.want) {
			t.Fatalf("%s: args %q, want %q", c.model, b, c.want)
		}
		if res.Model == "" {
			t.Fatalf("%s: the result does not say which model ran", c.model)
		}
	}
}

func TestAutoUsesAPinnedModel(t *testing.T) {
	h := newRunHarness(t)
	h.cfg.Providers.Settings["opencode"] = config.ProviderEntry{Model: "large"}
	res, err := h.svc().Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("mode?"), Provider: "opencode"}, RunOptions{Model: "auto"})
	if err != nil || !strings.Contains(res.Output, "model: large") || res.Model != "large" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestModelNamesForACPRuntimes(t *testing.T) {
	h := newRunHarness(t)
	ctx := context.Background()
	run := func(model string) (domain.Result, error) {
		return h.svc().Run(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("mode?"), Provider: "opencode"}, RunOptions{Model: model})
	}
	if res, err := run("large"); err != nil || !strings.Contains(res.Output, "model: large") || res.Model != "large" {
		t.Fatalf("named: %+v %v", res, err)
	}
	for _, kw := range []string{"latest", "auto"} {
		// The fake runtime offers no auto model and lists no dates, so both
		// keep its own default.
		res, err := run(kw)
		if err != nil || res.Model == "" || res.Model == kw {
			t.Fatalf("%s: %+v %v", kw, res, err)
		}
	}
	_, err := run("larg")
	if domain.CodeOf(err) != domain.ErrInvalidInput || !strings.Contains(err.Error(), "did you mean large?") || !strings.Contains(err.Error(), "offers small, large") {
		t.Fatalf("unknown model: %v", err)
	}
}
