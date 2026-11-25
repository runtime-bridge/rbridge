package service

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/rbridge/rbridge/internal/domain"
)

// CheckStatus is the outcome of one doctor check.
type CheckStatus string

const (
	CheckOK   CheckStatus = "ok"
	CheckWarn CheckStatus = "warn"
	CheckFail CheckStatus = "fail"
)

// Check is one line of the doctor report.
type Check struct {
	Name   string      `json:"name"`
	Status CheckStatus `json:"status"`
	Detail string      `json:"detail"`
	Fix    string      `json:"fix,omitempty"`
}

// Report is the result of Doctor.
type Report struct {
	Checks []Check `json:"checks"`
}

// Failed reports whether any check failed.
func (r Report) Failed() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFail {
			return true
		}
	}
	return false
}

var installHints = map[string]string{
	"claude-code": "Install Claude Code (https://code.claude.com/docs), then run `claude` once to sign in.",
	"opencode":    "Install OpenCode (https://opencode.ai/docs), then configure a model provider, for example a local Ollama server.",
	"codex":       "Optional. Install the ACP adapter: npm install -g @agentclientprotocol/codex-acp",
	"gemini-cli":  "Optional. Install Gemini CLI (https://www.geminicli.com) and set GEMINI_API_KEY.",
	"claude-acp":  "Optional, API key only. Install: npm install -g @agentclientprotocol/claude-agent-acp",
}

// optional providers do not count as missing when they are not installed.
var optionalProviders = map[string]bool{"codex": true, "gemini-cli": true, "claude-acp": true}

// Doctor checks configuration, directories and providers, always re-detecting.
func (s *Service) Doctor(ctx context.Context) Report {
	var r Report
	add := func(c Check) { r.Checks = append(r.Checks, c) }

	var found []string
	for _, l := range s.layers {
		if l.Found && l.Path != "" {
			found = append(found, l.Name+" ("+l.Path+")")
		}
		if len(l.Ignored) > 0 {
			add(Check{
				Name:   "config." + l.Name,
				Status: CheckWarn,
				Detail: "ignored keys locked by the system configuration: " + strings.Join(l.Ignored, ", "),
				Fix:    "Remove these keys from " + l.Path + ".",
			})
		}
	}
	detail := "built-in defaults only"
	if len(found) > 0 {
		detail = "loaded " + strings.Join(found, ", ")
	}
	add(Check{Name: "config", Status: CheckOK, Detail: detail})

	for _, d := range []struct{ name, dir string }{{"data directory", s.paths.DataDir}, {"cache directory", s.paths.CacheDir}} {
		name, dir := d.name, d.dir
		if err := ensureDir(dir); err != nil {
			add(Check{Name: name, Status: CheckFail, Detail: fmt.Sprintf("%s is not writable: %v", dir, err), Fix: "Fix the permissions, or set RBRIDGE_HOME to a writable directory."})
		} else {
			add(Check{Name: name, Status: CheckOK, Detail: dir})
		}
	}

	statuses, err := s.Providers(ctx, true)
	if err != nil {
		add(Check{Name: "providers", Status: CheckFail, Detail: err.Error()})
		return r
	}
	available := 0
	for _, st := range statuses {
		c := Check{Name: "provider " + st.ID}
		rt := st.Runtime
		switch rt.Health {
		case domain.HealthAvailable:
			available++
			c.Status = CheckOK
			c.Detail = fmt.Sprintf("%s %s at %s (auth: %s)", st.DisplayName, orDash(rt.Version), rt.Executable, rt.AuthMode)
		case domain.HealthDisabled:
			c.Status = CheckOK
			c.Detail = "disabled in configuration"
		case domain.HealthNeedsAPIKey:
			c.Status = CheckWarn
			c.Detail = fmt.Sprintf("%s %s is installed but needs an API key", st.DisplayName, orDash(rt.Version))
			c.Fix = strings.Join(rt.Notes, "; ")
		case domain.HealthMissing:
			c.Status = CheckWarn
			if optionalProviders[st.ID] {
				c.Status = CheckOK
			}
			c.Detail = "not installed"
			c.Fix = installHints[st.ID]
			if c.Fix == "" {
				c.Fix = "Check providers.custom launch.command for " + st.ID + "."
			}
		default:
			c.Status = CheckFail
			var errs []string
			for _, in := range rt.Installations {
				if in.Error != "" {
					errs = append(errs, in.Path+": "+in.Error)
				}
			}
			c.Detail = "found but not runnable: " + strings.Join(errs, "; ")
			c.Fix = "Reinstall it, or pin a working copy with providers." + st.ID + ".executable."
		}
		add(c)
	}
	if dir := s.getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		c := Check{Name: "claude accounts", Status: CheckWarn}
		match := ""
		for _, st := range statuses {
			for _, u := range st.Skills.User {
				if filepath.Clean(filepath.Dir(u)) == filepath.Clean(dir) && st.ID != "claude-code" {
					match = st.ID
				}
			}
		}
		if match != "" {
			c.Detail = fmt.Sprintf("your shell sets CLAUDE_CONFIG_DIR=%s, which is provider %s; rbridge's claude-code always uses ~/.claude", dir, match)
			c.Fix = "Use --provider " + match + ", or put it first in providers.order."
		} else {
			c.Detail = fmt.Sprintf("your shell sets CLAUDE_CONFIG_DIR=%s, which no provider uses; rbridge's claude-code always uses ~/.claude", dir)
			c.Fix = "Declare it under providers.claude-code.accounts to use it."
		}
		add(c)
	}
	if available == 0 {
		add(Check{Name: "runtimes", Status: CheckFail, Detail: "no agent runtime is available", Fix: "Install at least one runtime; see the provider lines above."})
	}
	return r
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
