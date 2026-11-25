package service

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/skills/lockfile"
	"github.com/runtime-bridge/rbridge/internal/skills/registry"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
)

const deliveryCollection = "deliveries"

// PermanentDelivery is a skill rbridge placed in a runtime's own skill folder
// at the user's request. rbridge tracks it so it can update or remove it, and
// never touches an entry it did not create.
type PermanentDelivery struct {
	SkillID  string    `json:"skill_id"`
	Provider string    `json:"provider"`
	Scope    string    `json:"scope"` // "user" or "project"
	Project  string    `json:"project,omitempty"`
	Path     string    `json:"path"`
	Digest   string    `json:"digest"`
	Mode     string    `json:"mode"` // "link", "copy" or "copy-no-scripts"
	Excluded string    `json:"git_exclude,omitempty"`
	At       time.Time `json:"at"`
}

// DeliverPermanent places a skill in the provider's native user folder, or in
// the project folder when project is set, and records it.
func (s *Service) DeliverPermanent(ctx context.Context, id, provider, project string) (PermanentDelivery, error) {
	m, ok := s.registry.Get(provider)
	if !ok {
		return PermanentDelivery{}, domain.Errorf(domain.ErrProviderNotFound, "no provider %q", provider)
	}
	reg := s.Skills()
	rec, err := reg.Require(ctx, id)
	if err != nil {
		return PermanentDelivery{}, err
	}
	if rec.Trust == domain.TrustBlocked {
		return PermanentDelivery{}, domain.Errorf(domain.ErrSkillBlocked, "skill %s is blocked", id)
	}
	if err := reg.Verify(ctx, rec); err != nil {
		return PermanentDelivery{}, err
	}
	loc := s.skillLocations(m)
	d := PermanentDelivery{SkillID: id, Provider: m.ID, Scope: "user", Digest: rec.Digest, At: s.now()}
	var dir string
	if project != "" {
		if !filepath.IsAbs(project) {
			return d, domain.Errorf(domain.ErrInvalidInput, "project must be an absolute path")
		}
		if fi, err := os.Stat(project); err != nil || !fi.IsDir() {
			return d, domain.Errorf(domain.ErrInvalidInput, "project %s is not a directory", project)
		}
		if len(loc.Project) == 0 {
			return d, domain.Errorf(domain.ErrCapabilityUnavailable, "%s has no project skill folder", m.ID)
		}
		d.Scope, d.Project = "project", project
		dir = filepath.Join(project, filepath.FromSlash(loc.Project[0]))
	} else {
		if len(loc.User) == 0 {
			return d, domain.Errorf(domain.ErrCapabilityUnavailable, "%s has no user skill folder", m.ID)
		}
		dir = loc.User[0]
	}
	d.Path = filepath.Join(dir, id)

	prev, tracked, err := s.delivery(ctx, d.Path)
	if err != nil {
		return d, err
	}
	if _, err := os.Lstat(d.Path); err == nil {
		if !tracked {
			return d, domain.Errorf(domain.ErrInvalidInput, "%s already exists and was not created by rbridge; remove it yourself first", d.Path)
		}
		if err := os.RemoveAll(d.Path); err != nil {
			return d, err
		}
	}
	if tracked {
		d.Excluded = prev.Excluded
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return d, err
	}
	src := reg.Path(rec)
	sk, err := spec.Parse(src, reg.Limits)
	if err != nil {
		return d, &domain.Error{Code: domain.ErrSkillInvalid, Message: err.Error(), Cause: err}
	}
	switch {
	case rec.Trust.ScriptsAllowed() || len(sk.Scripts) == 0:
		if err := os.Symlink(src, d.Path); err == nil {
			d.Mode = "link"
		} else if err := registry.CopySkill(src, d.Path, nil); err != nil {
			return d, err
		} else {
			d.Mode = "copy"
		}
	default:
		skip := func(rel string) bool {
			return rel == "scripts" || strings.HasPrefix(rel, "scripts/") || slices.Contains(sk.Scripts, rel)
		}
		if err := registry.CopySkill(src, d.Path, skip); err != nil {
			return d, err
		}
		d.Mode = "copy-no-scripts"
	}
	if d.Scope == "project" && d.Excluded == "" {
		if line, err := gitExclude(project, d.Path); err == nil {
			d.Excluded = line
		}
	}
	if err := s.store.Put(ctx, deliveryCollection, deliveryKey(d.Path), d); err != nil {
		return d, err
	}
	return d, nil
}

// deliveryKey names a delivery in the store, whose keys cannot hold paths.
func deliveryKey(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "d-" + hex.EncodeToString(sum[:16])
}

func (s *Service) delivery(ctx context.Context, path string) (PermanentDelivery, bool, error) {
	var d PermanentDelivery
	ok, err := s.store.Get(ctx, deliveryCollection, deliveryKey(path), &d)
	return d, ok, err
}

// PermanentDeliveries lists tracked deliveries, optionally for one skill.
func (s *Service) PermanentDeliveries(ctx context.Context, skillID string) ([]PermanentDelivery, error) {
	keys, err := s.store.List(ctx, deliveryCollection)
	if err != nil {
		return nil, err
	}
	out := []PermanentDelivery{}
	for _, k := range keys {
		var d PermanentDelivery
		if ok, err := s.store.Get(ctx, deliveryCollection, k, &d); err == nil && ok && (skillID == "" || d.SkillID == skillID) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Undeliver removes tracked deliveries of a skill. An empty provider means
// every provider; an empty project means user folders and every project.
func (s *Service) Undeliver(ctx context.Context, skillID, provider, project string) ([]PermanentDelivery, error) {
	all, err := s.PermanentDeliveries(ctx, skillID)
	if err != nil {
		return nil, err
	}
	var removed []PermanentDelivery
	for _, d := range all {
		if (provider != "" && d.Provider != provider) || (project != "" && d.Project != project) {
			continue
		}
		if err := removeDelivered(d); err != nil {
			return removed, err
		}
		if d.Excluded != "" {
			_ = gitUnexclude(d.Project, d.Excluded)
		}
		if err := s.store.Delete(ctx, deliveryCollection, deliveryKey(d.Path)); err != nil {
			return removed, err
		}
		removed = append(removed, d)
	}
	return removed, nil
}

// removeDelivered deletes what rbridge put at d.Path. A link is removed only
// when it still points into rbridge's library; a copy only while it still
// holds the skill's SKILL.md.
func removeDelivered(d PermanentDelivery) error {
	fi, err := os.Lstat(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return os.Remove(d.Path)
	}
	if _, err := os.Stat(filepath.Join(d.Path, "SKILL.md")); err != nil {
		return domain.Errorf(domain.ErrInvalidInput, "%s no longer looks like the delivered skill; leaving it in place", d.Path)
	}
	return os.RemoveAll(d.Path)
}

// SetSkillTrust changes a skill's trust and brings its permanent deliveries
// in line: blocked skills are removed, others are laid out again so scripts
// follow the new trust.
func (s *Service) SetSkillTrust(ctx context.Context, id string, t domain.TrustState) (domain.SkillRecord, error) {
	rec, err := s.Skills().SetTrust(ctx, id, t)
	if err != nil {
		return rec, err
	}
	if t == domain.TrustBlocked {
		_, err = s.Undeliver(ctx, id, "", "")
		return rec, err
	}
	ds, err := s.PermanentDeliveries(ctx, id)
	if err != nil {
		return rec, err
	}
	for _, d := range ds {
		if _, err := s.DeliverPermanent(ctx, id, d.Provider, d.Project); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// RemoveSkill removes a skill and every permanent delivery of it.
func (s *Service) RemoveSkill(ctx context.Context, id string) error {
	if _, err := s.Skills().Require(ctx, id); err != nil {
		return err
	}
	if _, err := s.Undeliver(ctx, id, "", ""); err != nil {
		return err
	}
	return s.Skills().Remove(ctx, id)
}

// gitExclude adds path to the project's .git/info/exclude when the project
// is a Git work tree, so a delivered skill never shows up as a change.
// It returns the line it added.
func gitExclude(project, path string) (string, error) {
	gitDir := filepath.Join(project, ".git")
	if fi, err := os.Stat(gitDir); err != nil || !fi.IsDir() {
		return "", errors.New("not a git work tree")
	}
	rel, err := filepath.Rel(project, path)
	if err != nil {
		return "", err
	}
	line := "/" + filepath.ToSlash(rel)
	file := filepath.Join(gitDir, "info", "exclude")
	if lines, _ := readLines(file); slices.Contains(lines, line) {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, _ := f.Stat(); fi != nil && fi.Size() > 0 {
		b, _ := os.ReadFile(file)
		if len(b) > 0 && b[len(b)-1] != '\n' {
			line = "\n" + line
		}
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		return "", err
	}
	return strings.TrimPrefix(line, "\n"), nil
}

func gitUnexclude(project, line string) error {
	file := filepath.Join(project, ".git", "info", "exclude")
	lines, err := readLines(file)
	if err != nil {
		return err
	}
	kept := slices.DeleteFunc(lines, func(l string) bool { return l == line })
	out := strings.Join(kept, "\n")
	if len(kept) > 0 {
		out += "\n"
	}
	return os.WriteFile(file, []byte(out), 0o644)
}

func readLines(file string) ([]string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

const projectLinkCollection = "projectlinks"

// projectLink is a skill linked into a project for one session.
type projectLink struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
	Path      string `json:"path"`
	Excluded  string `json:"git_exclude,omitempty"`
}

func (s *Service) trackProjectLinks(ctx context.Context, session, workspace string, links []string) error {
	for _, l := range links {
		pl := projectLink{Session: session, Workspace: workspace, Path: l}
		if line, err := gitExclude(workspace, l); err == nil {
			pl.Excluded = line
		}
		if err := s.store.Put(ctx, projectLinkCollection, deliveryKey(l), pl); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) projectLinks(ctx context.Context) []projectLink {
	keys, _ := s.store.List(ctx, projectLinkCollection)
	var out []projectLink
	for _, k := range keys {
		var pl projectLink
		if ok, _ := s.store.Get(ctx, projectLinkCollection, k, &pl); ok {
			out = append(out, pl)
		}
	}
	return out
}

func (s *Service) dropProjectLink(ctx context.Context, pl projectLink) {
	removeProjectLinks([]string{pl.Path})
	if pl.Excluded != "" {
		_ = gitUnexclude(pl.Workspace, pl.Excluded)
	}
	_ = s.store.Delete(ctx, projectLinkCollection, deliveryKey(pl.Path))
}

// untrackProjectLinks removes the links a session placed in its project.
func (s *Service) untrackProjectLinks(session string) {
	ctx := context.Background()
	for _, pl := range s.projectLinks(ctx) {
		if pl.Session == session {
			s.dropProjectLink(ctx, pl)
		}
	}
}

// reapProjectLinks removes links left in a workspace by sessions that are no
// longer running, for example after a crash.
func (s *Service) reapProjectLinks(ctx context.Context, workspace string) {
	for _, pl := range s.projectLinks(ctx) {
		if pl.Workspace != workspace {
			continue
		}
		rec, err := s.SessionRecord(ctx, pl.Session)
		if err == nil {
			switch rec.State {
			case domain.StateClosed, domain.StateFailed, domain.StateInterrupted:
			default:
				if s.live(rec) {
					continue
				}
			}
		}
		s.dropProjectLink(ctx, pl)
	}
}

// removeProjectLinks deletes links, or copies made where links are not
// available, that rbridge placed in a project.
func removeProjectLinks(paths []string) {
	for _, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			os.Remove(p)
		} else {
			os.RemoveAll(p)
		}
	}
}

// LockImport is the outcome for one entry of a skills-lock.json.
type LockImport struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "imported", "unchanged", "skipped" or "refused"
	From   string `json:"from,omitempty"`
	Reason string `json:"reason,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// ImportLock adds the skills listed in a project's skills-lock.json to the
// library. An installed copy under .agents/skills is used when its files
// match the lock's hash; otherwise a Git source pinned to a commit is
// fetched when fetch is true and checked against the same hash. Entries
// that cannot be verified are skipped. Imported skills start unreviewed.
func (s *Service) ImportLock(ctx context.Context, project string, fetch bool) ([]LockImport, error) {
	path := filepath.Join(project, lockfile.Name)
	l, err := lockfile.Read(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, domain.Errorf(domain.ErrInvalidInput, "no %s in %s", lockfile.Name, project)
		}
		return nil, domain.Errorf(domain.ErrInvalidInput, "%s: %v", path, err)
	}
	names := make([]string, 0, len(l.Skills))
	for n := range l.Skills {
		names = append(names, n)
	}
	sort.Strings(names)
	reg := s.Skills()
	var out []LockImport
	for _, name := range names {
		e := l.Skills[name]
		res := LockImport{Name: name}
		if e.ComputedHash == "" {
			res.Status, res.Reason = "skipped", "the lock has no content hash for it (a version 1 lock); add it yourself after reviewing it"
			out = append(out, res)
			continue
		}
		check := func(dir string) error {
			got, err := lockfile.Hash(dir)
			if err != nil {
				return err
			}
			if got != e.ComputedHash {
				return domain.Errorf(domain.ErrSkillDigestMismatch, "files do not match the lock's hash")
			}
			return nil
		}
		var req registry.AddRequest
		installed := filepath.Join(project, ".agents", "skills", name)
		url, subdir, pinned := e.GitURL()
		switch {
		case dirExists(installed):
			req = registry.AddRequest{Path: installed, Check: check, Lock: path}
			res.From = installed
		case pinned && fetch:
			req = registry.AddRequest{URL: url, Rev: e.Commit, Subdir: subdir, Check: check, Lock: path}
			res.From = url + "@" + e.Commit
			if subdir != "" {
				res.From += " " + subdir
			}
		case pinned:
			res.Status, res.Reason = "skipped", "not installed in the project, and fetching is off"
			out = append(out, res)
			continue
		default:
			res.Status, res.Reason = "skipped", "not installed in the project, and its "+e.SourceType+" source is not pinned to a Git commit"
			out = append(out, res)
			continue
		}
		added, err := reg.Add(ctx, req)
		switch {
		case err != nil:
			res.Status, res.Reason = "refused", err.Error()
		case added.Unchanged:
			res.Status, res.Digest = "unchanged", added.Record.Digest
		default:
			res.Status, res.Digest = "imported", added.Record.Digest
			if added.Record.ID != name {
				res.Reason = "installed as " + added.Record.ID + ", the name in its SKILL.md"
			}
		}
		out = append(out, res)
	}
	return out, nil
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
