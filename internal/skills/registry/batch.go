package registry

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
)

// maxFindDepth bounds how far below a source folder skills are looked for.
const maxFindDepth = 6

// FindSkills returns the skill folders at or under root: root itself when it
// holds a SKILL.md, otherwise every folder below it that does. It does not
// look inside a skill, nor into hidden folders or node_modules.
func FindSkills(root string) ([]string, error) {
	info, err := os.Stat(root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, domain.Errorf(domain.ErrInvalidInput, "%s does not exist", root)
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, domain.Errorf(domain.ErrInvalidInput, "%s is not a folder", root)
	}
	if isSkillDir(root) {
		return []string{root}, nil
	}
	var found []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || p == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Count(filepath.ToSlash(rel), "/")+1 > maxFindDepth {
			return filepath.SkipDir
		}
		if isSkillDir(p) {
			found = append(found, p)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	return found, nil
}

func isSkillDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && info.Mode().IsRegular()
}

// AddManyRequest adds every skill found in one or more sources in one go.
type AddManyRequest struct {
	// Paths are local folders, each a skill or a folder holding skills.
	Paths []string
	Link  bool
	// URL and Rev name a Git repository; Subdirs are folders inside it, each
	// a skill or a folder holding skills. No Subdirs means the whole repository.
	URL     string
	Rev     string
	Subdirs []string
	// Only keeps the skills whose names match one of these, and Exclude leaves
	// out the ones that match; both take names or path.Match patterns.
	Only    []string
	Exclude []string
	// DryRun finds and checks the skills without installing any.
	DryRun bool
	// SetTrust, when set, is called for each skill added, updated or
	// already installed with the same content.
	SetTrust func(ctx context.Context, id string) (domain.SkillRecord, error)
}

// AddManyItem is what happened to one skill folder.
type AddManyItem struct {
	ID string `json:"id,omitempty"`
	// Source is the local folder or, for Git, the folder inside the repository.
	Source string `json:"source"`
	// Status is added, updated, unchanged, would_add, skipped or failed.
	Status string     `json:"status"`
	Result *AddResult `json:"result,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

// AddManyResult is the outcome of AddMany, one item per skill folder found.
type AddManyResult struct {
	Items    []AddManyItem `json:"items"`
	Warnings []string      `json:"warnings,omitempty"`
	DryRun   bool          `json:"dry_run,omitempty"`
}

// Failed counts the items that could not be added.
func (r AddManyResult) Failed() int {
	n := 0
	for _, it := range r.Items {
		if it.Status == "failed" {
			n++
		}
	}
	return n
}

type candidate struct {
	dir, source, subdir string
	name                string
	sk                  spec.Skill
	err                 error
}

// AddMany finds the skills in every source and adds the ones the filters
// keep. One skill failing does not stop the others; each item says what
// happened to it. Errors are returned only for a problem with the request
// itself, such as a source that does not exist or a bad pattern.
func (r *Registry) AddMany(ctx context.Context, req AddManyRequest) (AddManyResult, error) {
	for _, p := range append(append([]string{}, req.Only...), req.Exclude...) {
		if _, err := path.Match(p, ""); err != nil {
			return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "bad skill pattern %q", p)
		}
	}
	if req.URL != "" && len(req.Paths) > 0 {
		return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "give local folders or a Git URL, not both")
	}
	if req.URL == "" && len(req.Paths) == 0 {
		return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "a folder or a Git URL is required")
	}

	var cands []candidate
	var repo, commit string
	if req.URL != "" {
		subdirs := req.Subdirs
		if len(subdirs) == 0 {
			subdirs = []string{""}
		}
		for _, s := range subdirs {
			if err := checkSubdir(s); err != nil {
				return AddManyResult{}, err
			}
		}
		var cleanup func()
		var err error
		repo, commit, cleanup, err = r.fetchGit(ctx, req.URL, req.Rev)
		if err != nil {
			return AddManyResult{}, err
		}
		defer cleanup()
		for _, s := range subdirs {
			dirs, err := FindSkills(filepath.Join(repo, filepath.FromSlash(s)))
			if err != nil {
				return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "subdir %s is not a folder in the repository", s)
			}
			if len(dirs) == 0 {
				return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "no skills found in %s of the repository (a skill is a folder with a SKILL.md)", displaySubdir(s))
			}
			for _, d := range dirs {
				rel, _ := filepath.Rel(repo, d)
				rel = filepath.ToSlash(rel)
				if rel == "." {
					rel = ""
				}
				cands = append(cands, candidate{dir: d, source: displaySubdir(rel), subdir: rel})
			}
		}
	} else {
		for _, p := range req.Paths {
			abs, err := filepath.Abs(p)
			if err != nil {
				return AddManyResult{}, err
			}
			dirs, err := FindSkills(abs)
			if err != nil {
				return AddManyResult{}, err
			}
			if len(dirs) == 0 {
				return AddManyResult{}, domain.Errorf(domain.ErrInvalidInput, "no skills found in %s (a skill is a folder with a SKILL.md)", abs)
			}
			for _, d := range dirs {
				cands = append(cands, candidate{dir: d, source: d})
			}
		}
	}

	// Read every skill's name first, so filters that name nothing are
	// reported before anything is installed.
	seenDir := map[string]bool{}
	unique := cands[:0]
	for _, c := range cands {
		if seenDir[c.dir] {
			continue
		}
		seenDir[c.dir] = true
		c.sk, c.err = spec.Parse(c.dir, r.Limits)
		c.name = filepath.Base(c.dir)
		if c.err == nil {
			c.name = c.sk.Name
		}
		unique = append(unique, c)
	}
	cands = unique
	out := AddManyResult{DryRun: req.DryRun}
	for _, p := range req.Only {
		if !matchesAny(cands, p) {
			return AddManyResult{}, domain.Errorf(domain.ErrSkillNotFound, "--only %s matches none of the %d skills found", p, len(cands))
		}
	}
	for _, p := range req.Exclude {
		if !matchesAny(cands, p) {
			out.Warnings = append(out.Warnings, "--exclude "+p+" matched no skill")
		}
	}
	seen := map[string]string{}
	for _, c := range cands {
		item := AddManyItem{Source: c.source, ID: c.name}
		name, err := c.name, c.err
		if len(req.Only) > 0 && firstMatch(req.Only, name) == "" {
			item.Status, item.Reason = "skipped", "not named by --only"
			out.Items = append(out.Items, item)
			continue
		}
		if p := firstMatch(req.Exclude, name); p != "" {
			item.Status, item.Reason = "skipped", "excluded by "+p
			out.Items = append(out.Items, item)
			continue
		}
		if err != nil {
			item.Status, item.Reason = "failed", invalid(err).Error()
			out.Items = append(out.Items, item)
			continue
		}
		if prev, dup := seen[name]; dup {
			item.Status, item.Reason = "failed", "another folder in this command, "+prev+", is also skill "+name
			out.Items = append(out.Items, item)
			continue
		}
		seen[name] = c.source
		if req.DryRun {
			item.Status = "would_add"
			out.Items = append(out.Items, item)
			continue
		}
		var res AddResult
		if req.URL != "" {
			res, err = r.installGitDir(ctx, AddRequest{URL: req.URL}, repo, commit, c.subdir)
		} else {
			res, err = r.addLocal(ctx, AddRequest{Path: c.dir, Link: req.Link})
		}
		if err != nil {
			item.Status, item.Reason = "failed", err.Error()
			out.Items = append(out.Items, item)
			continue
		}
		switch {
		case res.Unchanged:
			item.Status = "unchanged"
		case res.Replaced != "":
			item.Status = "updated"
		default:
			item.Status = "added"
		}
		if req.SetTrust != nil {
			rec, err := req.SetTrust(ctx, res.Record.ID)
			if err != nil {
				item.Status, item.Reason = "failed", "added, but setting its trust failed: "+err.Error()
			} else {
				res.Record = rec
			}
		}
		item.ID = res.Record.ID
		item.Result = &res
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func matchesAny(cands []candidate, pattern string) bool {
	for _, c := range cands {
		if ok, _ := path.Match(pattern, c.name); ok {
			return true
		}
	}
	return false
}

func firstMatch(patterns []string, name string) string {
	for _, p := range patterns {
		if ok, _ := path.Match(p, name); ok {
			return p
		}
	}
	return ""
}

func displaySubdir(s string) string {
	if s == "" {
		return "the root"
	}
	return s
}

// MatchIDs resolves names and path.Match patterns against installed skill
// ids. Every name or pattern must match at least one skill.
func MatchIDs(installed []string, patterns []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range patterns {
		if _, err := path.Match(p, ""); err != nil {
			return nil, domain.Errorf(domain.ErrInvalidInput, "bad skill pattern %q", p)
		}
		hit := false
		for _, id := range installed {
			if ok, _ := path.Match(p, id); ok {
				hit = true
				if !seen[id] {
					seen[id] = true
					out = append(out, id)
				}
			}
		}
		if !hit {
			return nil, domain.Errorf(domain.ErrSkillNotFound, "no installed skill matches %q", p)
		}
	}
	return out, nil
}
