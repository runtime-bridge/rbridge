// Package registry keeps rbridge's skill library: where each skill came from,
// its digest and its trust state.
package registry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
	"github.com/runtime-bridge/rbridge/internal/store"
)

const collection = "skills"

// Registry is the skill library under one data directory.
type Registry struct {
	Dir    string // <data dir>/skills
	Store  store.Store
	Limits spec.Limits
	Now    func() time.Time
	// Git runs git; it defaults to process.Run with a filtered environment.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// RequirePinned rejects Git revisions that are not full commit hashes.
	RequirePinned bool
}

// AddRequest describes where a skill comes from.
type AddRequest struct {
	Path string // local directory
	Link bool   // use Path in place instead of copying it
	URL  string // Git repository
	Rev  string // Git revision
	// Subdir is the skill directory inside the repository.
	Subdir string
	// Check, when set, inspects the skill's source folder before anything is
	// installed; an error stops the add.
	Check func(dir string) error
	// Lock records the lock file the skill was imported from.
	Lock string
}

// AddResult is the outcome of Add.
type AddResult struct {
	Record   domain.SkillRecord `json:"record"`
	Warnings []string           `json:"warnings,omitempty"`
	// Unchanged is true when the same content was already installed.
	Unchanged bool `json:"unchanged,omitempty"`
	// Replaced is the digest of the version this one replaced.
	Replaced string `json:"replaced,omitempty"`
}

// Add validates a skill and installs it into the library.
func (r *Registry) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	switch {
	case req.URL != "" && req.Path != "":
		return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "give a path or a Git URL, not both")
	case req.URL != "":
		return r.addGit(ctx, req)
	case req.Path != "":
		return r.addLocal(ctx, req)
	}
	return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "a path or a Git URL is required")
}

func (r *Registry) addLocal(ctx context.Context, req AddRequest) (AddResult, error) {
	src, err := filepath.Abs(req.Path)
	if err != nil {
		return AddResult{}, err
	}
	sk, err := spec.Parse(src, r.Limits)
	if err != nil {
		return AddResult{}, invalid(err)
	}
	if req.Check != nil {
		if err := req.Check(src); err != nil {
			return AddResult{}, err
		}
	}
	source := domain.SkillSource{Type: "local", Path: src, Lock: req.Lock}
	if req.Link {
		source.Type = "link"
		return r.install(ctx, sk, source, "")
	}
	return r.install(ctx, sk, source, src)
}

var fullHash = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

func (r *Registry) addGit(ctx context.Context, req AddRequest) (AddResult, error) {
	if !strings.HasPrefix(req.URL, "https://") && !strings.HasPrefix(req.URL, "file://") && !strings.HasPrefix(req.URL, "ssh://") && !strings.HasPrefix(req.URL, "git@") {
		return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "Git URL must use https, ssh or file")
	}
	if req.Rev == "" {
		return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "a Git revision is required (--rev)")
	}
	if r.RequirePinned && !fullHash.MatchString(req.Rev) {
		return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "Git revision must be a full commit hash; set skills.git_require_pinned_revision: false to allow branches and tags")
	}
	if strings.Contains(req.Subdir, "..") || filepath.IsAbs(req.Subdir) {
		return AddResult{}, domain.Errorf(domain.ErrInvalidInput, "subdir must be a relative path inside the repository")
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return AddResult{}, err
	}
	tmp, err := os.MkdirTemp(r.Dir, ".git-")
	if err != nil {
		return AddResult{}, err
	}
	defer os.RemoveAll(tmp)
	repo := filepath.Join(tmp, "repo")
	steps := [][]string{
		{"init", "-q", repo},
		{"-C", repo, "remote", "add", "origin", req.URL},
		{"-C", repo, "fetch", "-q", "--depth", "1", "origin", req.Rev},
		{"-C", repo, "-c", "advice.detachedHead=false", "checkout", "-q", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if _, err := r.git(ctx, tmp, s...); err != nil {
			return AddResult{}, err
		}
	}
	commit, err := r.git(ctx, tmp, "-C", repo, "rev-parse", "HEAD")
	if err != nil {
		return AddResult{}, err
	}
	dir := filepath.Join(repo, filepath.FromSlash(req.Subdir))
	sk, err := spec.Parse(dir, r.Limits)
	if err != nil {
		return AddResult{}, invalid(err)
	}
	if req.Check != nil {
		if err := req.Check(dir); err != nil {
			return AddResult{}, err
		}
	}
	return r.install(ctx, sk, domain.SkillSource{Type: "git", URL: req.URL, Revision: strings.TrimSpace(commit), Subdir: req.Subdir, Lock: req.Lock}, dir)
}

func (r *Registry) git(ctx context.Context, dir string, args ...string) (string, error) {
	if r.Git != nil {
		return r.Git(ctx, dir, args...)
	}
	return RunGit(ctx, dir, args...)
}

// RunGit runs git with a minimal environment and no interactive prompts.
func RunGit(ctx context.Context, dir string, args ...string) (string, error) {
	path, err := lookGit()
	if err != nil {
		return "", domain.Errorf(domain.ErrProviderUnavailable, "git is required to add skills from a repository")
	}
	env := process.FilterEnv(os.Environ(), []string{"PATH", "HOME", "USERPROFILE", "SystemRoot", "TMPDIR", "TEMP", "TMP", "SSH_AUTH_SOCK", "LANG"}, map[string]string{"GIT_TERMINAL_PROMPT": "0"})
	out, err := process.Run(ctx, process.Spec{Path: path, Args: args, Dir: dir, Env: env, Timeout: 5 * time.Minute, MaxOutput: 1 << 20})
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 {
		return "", domain.Errorf(domain.ErrExecutionFailed, "git %s failed: %s", args[len(args)-1], strings.TrimSpace(string(out.Stderr)))
	}
	return string(out.Stdout), nil
}

var lookGit = func() (string, error) { return execLookPath("git") }

// install records the skill. copyFrom is empty for linked skills.
func (r *Registry) install(ctx context.Context, sk spec.Skill, source domain.SkillSource, copyFrom string) (AddResult, error) {
	res := AddResult{Warnings: sk.Warnings}
	prev, had, err := r.Get(ctx, sk.Name)
	if err != nil {
		return res, err
	}
	if had && prev.Digest == sk.Digest && prev.Source == source {
		res.Record, res.Unchanged = prev, true
		return res, nil
	}
	if copyFrom != "" {
		dest := r.libraryPath(sk.Name, sk.Digest)
		if _, err := os.Stat(dest); err != nil {
			if err := copySkill(copyFrom, dest, nil); err != nil {
				return res, err
			}
		}
		got, err := spec.Digest(dest, r.Limits)
		if err != nil || got != sk.Digest {
			os.RemoveAll(filepath.Dir(dest))
			return res, domain.Errorf(domain.ErrSkillDigestMismatch, "copied skill does not match its source")
		}
	}
	rec := domain.SkillRecord{
		ID: sk.Name, Version: sk.Version(), Description: sk.Description, Source: source,
		Digest: sk.Digest, License: sk.License, Trust: domain.TrustUnreviewed,
		HasScripts: sk.HasScripts(), InstalledAt: r.now(),
	}
	if had && prev.Digest == sk.Digest {
		// Same content from a new source keeps its review.
		rec.Trust = prev.Trust
	}
	if err := r.Store.Put(ctx, collection, rec.ID, rec); err != nil {
		return res, err
	}
	if had && prev.Digest != sk.Digest {
		res.Replaced = prev.Digest
		if prev.Source.Type != "link" {
			os.RemoveAll(filepath.Dir(r.libraryPath(prev.ID, prev.Digest)))
		}
	}
	res.Record = rec
	return res, nil
}

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now().UTC()
}

// libraryPath keeps the final directory named after the skill, as runtimes require.
func (r *Registry) libraryPath(id, digest string) string {
	short := strings.TrimPrefix(digest, "sha256:")
	if len(short) > 16 {
		short = short[:16]
	}
	return filepath.Join(r.Dir, id, short, id)
}

// Path returns the directory holding the installed skill.
func (r *Registry) Path(rec domain.SkillRecord) string {
	if rec.Source.Type == "link" {
		return rec.Source.Path
	}
	return r.libraryPath(rec.ID, rec.Digest)
}

// Get returns one skill.
func (r *Registry) Get(ctx context.Context, id string) (domain.SkillRecord, bool, error) {
	if !spec.ValidName(id) {
		return domain.SkillRecord{}, false, nil
	}
	var rec domain.SkillRecord
	ok, err := r.Store.Get(ctx, collection, id, &rec)
	return rec, ok, err
}

// Require returns a skill or a skill_not_found error.
func (r *Registry) Require(ctx context.Context, id string) (domain.SkillRecord, error) {
	rec, ok, err := r.Get(ctx, id)
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, domain.Errorf(domain.ErrSkillNotFound, "no skill %q; add it with rbridge skills add", id)
	}
	return rec, nil
}

// List returns every skill sorted by id.
func (r *Registry) List(ctx context.Context) ([]domain.SkillRecord, error) {
	keys, err := r.Store.List(ctx, collection)
	if err != nil {
		return nil, err
	}
	out := make([]domain.SkillRecord, 0, len(keys))
	for _, k := range keys {
		var rec domain.SkillRecord
		if ok, err := r.Store.Get(ctx, collection, k, &rec); err == nil && ok {
			out = append(out, rec)
		}
	}
	return out, nil
}

var trustStates = []domain.TrustState{domain.TrustUnreviewed, domain.TrustReviewed, domain.TrustTrusted, domain.TrustRestricted, domain.TrustBlocked}

// ParseTrust validates a trust state name.
func ParseTrust(s string) (domain.TrustState, error) {
	for _, t := range trustStates {
		if string(t) == s {
			return t, nil
		}
	}
	return "", domain.Errorf(domain.ErrInvalidInput, "trust must be one of unreviewed, reviewed, trusted, restricted, blocked")
}

// SetTrust changes a skill's trust state after checking its content is unchanged.
func (r *Registry) SetTrust(ctx context.Context, id string, t domain.TrustState) (domain.SkillRecord, error) {
	rec, err := r.Require(ctx, id)
	if err != nil {
		return rec, err
	}
	if t != domain.TrustBlocked {
		if err := r.Verify(ctx, rec); err != nil {
			return rec, err
		}
	}
	rec.Trust = t
	return rec, r.Store.Put(ctx, collection, rec.ID, rec)
}

// Verify recomputes the digest of the installed skill.
func (r *Registry) Verify(_ context.Context, rec domain.SkillRecord) error {
	got, err := spec.Digest(r.Path(rec), r.Limits)
	if err != nil {
		var ve *spec.ValidationError
		if errors.As(err, &ve) {
			return &domain.Error{Code: domain.ErrSkillInvalid, Message: err.Error(), Cause: err}
		}
		if errors.Is(err, fs.ErrNotExist) {
			return domain.Errorf(domain.ErrSkillDigestMismatch, "skill %s is missing from %s", rec.ID, r.Path(rec))
		}
		return err
	}
	if got != rec.Digest {
		return &domain.Error{Code: domain.ErrSkillDigestMismatch, Message: fmt.Sprintf("skill %s changed since it was added; add it again to review the new content", rec.ID), Details: map[string]string{"recorded": rec.Digest, "current": got}}
	}
	return nil
}

// Remove deletes a skill and its library copy. Linked sources are left alone.
func (r *Registry) Remove(ctx context.Context, id string) error {
	rec, err := r.Require(ctx, id)
	if err != nil {
		return err
	}
	if rec.Source.Type != "link" {
		if err := os.RemoveAll(filepath.Join(r.Dir, rec.ID)); err != nil {
			return err
		}
	}
	return r.Store.Delete(ctx, collection, id)
}

func invalid(err error) error {
	var ve *spec.ValidationError
	if errors.As(err, &ve) {
		return &domain.Error{Code: domain.ErrSkillInvalid, Message: err.Error(), Cause: err}
	}
	return err
}

// CopySkill copies a skill tree to dest. Files for which skip returns true are
// left out. Symlinks are copied as links; Parse has already checked they stay inside.
func CopySkill(src, dest string, skip func(rel string) bool) error {
	return copySkill(src, dest, skip)
}

func copySkill(src, dest string, skip func(rel string) bool) error {
	tmp := dest + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		slash := filepath.ToSlash(rel)
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if slash != "." && skip != nil && skip(slash) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(tmp, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case info.Mode()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			return copyFile(p, target, info.Mode().Perm())
		}
	})
	if err != nil {
		os.RemoveAll(tmp)
		return err
	}
	os.RemoveAll(dest)
	return os.Rename(tmp, dest)
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
