// Package deliver makes installed skills visible to one runtime session
// without writing into the project or the user's own skill folders.
package deliver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/skills/registry"
)

// Strategy names how a runtime is given per-session skills.
type Strategy string

const (
	// SessionDir builds <scratch>/skills-session/<layout>/<name> and passes
	// <scratch>/skills-session to the runtime (Claude Code: --add-dir).
	SessionDir Strategy = "session-dir"
	// OpenCodeConfig writes an extra OpenCode config file listing a skills
	// path and points OPENCODE_CONFIG at it. OpenCode merges it with the
	// user's own configuration (spike, 2026-11-24).
	OpenCodeConfig Strategy = "opencode-config"
)

// Item is one skill to deliver.
type Item struct {
	Record domain.SkillRecord
	// Dir is the installed skill directory.
	Dir string
	// Scripts says whether the skill's scripts may be delivered.
	Scripts bool
	// ScriptFiles lists the files to leave out when Scripts is false.
	ScriptFiles []string
}

// Request is what Prepare needs.
type Request struct {
	Strategy Strategy
	// Layout is the path under the session directory for SessionDir.
	Layout  string
	Scratch string // private per-session directory
	Items   []Item
	// Getenv reads the caller's environment, to avoid overriding a user's
	// own OPENCODE_CONFIG.
	Getenv func(string) string
}

// Plan is the result of Prepare.
type Plan struct {
	SkillDirs  []string
	Env        map[string]string
	Deliveries []domain.SkillDelivery
	Scripts    map[string]bool
}

// Prepare lays out the skills for one session. Everything is created under
// Scratch, which the caller removes when the session ends.
func Prepare(req Request) (Plan, error) {
	plan := Plan{Env: map[string]string{}, Scripts: map[string]bool{}}
	if len(req.Items) == 0 {
		return plan, nil
	}
	if !filepath.IsAbs(req.Scratch) {
		return plan, fmt.Errorf("deliver: scratch must be absolute")
	}
	var target string
	switch req.Strategy {
	case SessionDir:
		root := filepath.Join(req.Scratch, "skills-session")
		target = filepath.Join(root, filepath.FromSlash(req.Layout))
		plan.SkillDirs = []string{root}
	case OpenCodeConfig:
		getenv := req.Getenv
		if getenv == nil {
			getenv = os.Getenv
		}
		target = filepath.Join(req.Scratch, "opencode-skills")
		switch {
		case getenv("OPENCODE_CONFIG") == "":
			cfg := filepath.Join(req.Scratch, "opencode.json")
			b, _ := json.Marshal(map[string]any{
				"$schema": "https://opencode.ai/config.json",
				"skills":  map[string]any{"paths": []string{target}},
			})
			if err := os.MkdirAll(req.Scratch, 0o700); err != nil {
				return plan, err
			}
			if err := os.WriteFile(cfg, b, 0o600); err != nil {
				return plan, err
			}
			plan.Env["OPENCODE_CONFIG"] = cfg
		case getenv("OPENCODE_CONFIG_DIR") == "":
			dir := filepath.Join(req.Scratch, "opencode-config-dir")
			target = filepath.Join(dir, "skills")
			plan.Env["OPENCODE_CONFIG_DIR"] = dir
		default:
			return plan, domain.Errorf(domain.ErrSkillUnavailable, "OPENCODE_CONFIG and OPENCODE_CONFIG_DIR are both set; rbridge will not override them to deliver skills")
		}
	default:
		return plan, domain.Errorf(domain.ErrSkillUnavailable, "this runtime has no per-session skill delivery")
	}
	if err := os.MkdirAll(target, 0o700); err != nil {
		return plan, err
	}
	for _, it := range req.Items {
		dest := filepath.Join(target, it.Record.ID)
		mode := string(req.Strategy)
		if it.Scripts || len(it.ScriptFiles) == 0 {
			if err := os.Symlink(it.Dir, dest); err != nil {
				// Symlinks may be unavailable (Windows without privilege); copy instead.
				if err := registry.CopySkill(it.Dir, dest, nil); err != nil {
					return plan, err
				}
				mode += "+copy"
			} else {
				mode += "+link"
			}
		} else {
			skip := scriptFilter(it.ScriptFiles)
			if err := registry.CopySkill(it.Dir, dest, skip); err != nil {
				return plan, err
			}
			mode += "+no-scripts"
		}
		plan.Scripts[it.Record.ID] = it.Scripts || len(it.ScriptFiles) == 0
		plan.Deliveries = append(plan.Deliveries, domain.SkillDelivery{SkillID: it.Record.ID, Digest: it.Record.Digest, Mode: mode})
	}
	return plan, nil
}

func scriptFilter(files []string) func(string) bool {
	set := map[string]bool{}
	for _, f := range files {
		set[f] = true
	}
	return func(rel string) bool {
		return rel == "scripts" || strings.HasPrefix(rel, "scripts/") || set[rel]
	}
}

// Clash is another copy of a skill with the same name that the runtime may
// also discover.
type Clash struct {
	SkillID string `json:"skill_id"`
	Path    string `json:"path"`
}

// FindClashes looks for <dir>/<id>/SKILL.md in a runtime's native locations.
// Project directories are relative to workspace.
func FindClashes(ids []string, workspace string, project, user []string) []Clash {
	var out []Clash
	seen := map[string]bool{}
	var dirs []string
	for _, p := range project {
		dirs = append(dirs, filepath.Join(workspace, filepath.FromSlash(p)))
	}
	dirs = append(dirs, user...)
	for _, id := range ids {
		for _, d := range dirs {
			p := filepath.Join(d, id)
			if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err == nil && !seen[p] {
				seen[p] = true
				out = append(out, Clash{SkillID: id, Path: p})
			}
		}
	}
	return out
}
