// Package spec parses and validates Agent Skills (https://agentskills.io/specification).
package spec

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Limits bound what rbridge accepts as a skill.
type Limits struct {
	MaxFileBytes  int64
	MaxTotalBytes int64
	MaxFiles      int
}

// DefaultLimits are generous for instructions and small scripts.
var DefaultLimits = Limits{MaxFileBytes: 5 << 20, MaxTotalBytes: 50 << 20, MaxFiles: 2000}

// Skill is a parsed skill directory.
type Skill struct {
	Dir           string            `json:"dir"`
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	License       string            `json:"license,omitempty"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
	AllowedTools  string            `json:"allowed_tools,omitempty"`
	Body          string            `json:"-"`
	// Files lists every regular file, relative and slash-separated, sorted.
	Files []string `json:"files"`
	// Scripts lists files under scripts/ and any file with an executable bit.
	Scripts  []string `json:"scripts,omitempty"`
	Size     int64    `json:"size"`
	Digest   string   `json:"digest"`
	Warnings []string `json:"warnings,omitempty"`
}

// Version returns metadata.version when present.
func (s Skill) Version() string { return s.Metadata["version"] }

// HasScripts reports whether the skill ships executable code.
func (s Skill) HasScripts() bool { return len(s.Scripts) > 0 }

var namePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidName reports whether n is a valid skill name.
func ValidName(n string) bool { return len(n) >= 1 && len(n) <= 64 && namePattern.MatchString(n) }

// ValidationError lists every problem found, so a user can fix them at once.
type ValidationError struct {
	Dir      string
	Problems []string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid skill %s: %s", e.Dir, strings.Join(e.Problems, "; "))
}

var knownFields = []string{"name", "description", "license", "compatibility", "metadata", "allowed-tools"}

// Parse reads and validates the skill in dir.
func Parse(dir string, lim Limits) (Skill, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Skill{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return Skill{}, err
	}
	if !fi.IsDir() {
		return Skill{}, fmt.Errorf("%s is not a directory", dir)
	}
	sk := Skill{Dir: abs}
	var problems []string

	raw, err := os.ReadFile(filepath.Join(abs, "SKILL.md"))
	if err != nil {
		return Skill{}, &ValidationError{Dir: dir, Problems: []string{"SKILL.md is missing"}}
	}
	front, body, err := splitFrontmatter(raw)
	if err != nil {
		return Skill{}, &ValidationError{Dir: dir, Problems: []string{err.Error()}}
	}
	sk.Body = body
	var fields map[string]any
	if err := yaml.Unmarshal(front, &fields); err != nil {
		return Skill{}, &ValidationError{Dir: dir, Problems: []string{"frontmatter is not valid YAML: " + err.Error()}}
	}
	for k := range fields {
		if !slices.Contains(knownFields, k) {
			sk.Warnings = append(sk.Warnings, "unknown frontmatter field "+k)
		}
	}
	str := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok || v == nil {
			return "", false
		}
		s, isStr := v.(string)
		if !isStr {
			problems = append(problems, key+" must be a string")
			return "", false
		}
		return s, true
	}

	sk.Name, _ = str("name")
	switch {
	case sk.Name == "":
		problems = append(problems, "name is required")
	case !ValidName(sk.Name):
		problems = append(problems, fmt.Sprintf("name %q must be 1 to 64 lowercase letters, digits and single hyphens", sk.Name))
	case sk.Name != filepath.Base(abs):
		problems = append(problems, fmt.Sprintf("name %q must match the directory name %q", sk.Name, filepath.Base(abs)))
	}
	sk.Description, _ = str("description")
	switch n := len([]rune(strings.TrimSpace(sk.Description))); {
	case n == 0:
		problems = append(problems, "description is required")
	case n > 1024:
		problems = append(problems, "description is longer than 1024 characters")
	}
	sk.License, _ = str("license")
	if c, ok := str("compatibility"); ok {
		sk.Compatibility = c
		if n := len([]rune(c)); n < 1 || n > 500 {
			problems = append(problems, "compatibility must be 1 to 500 characters")
		}
	}
	if v, ok := fields["metadata"]; ok && v != nil {
		m, isMap := v.(map[string]any)
		if !isMap {
			problems = append(problems, "metadata must be a map of strings")
		} else {
			sk.Metadata = map[string]string{}
			for k, x := range m {
				s, isStr := x.(string)
				if !isStr {
					problems = append(problems, "metadata."+k+" must be a string")
					continue
				}
				sk.Metadata[k] = s
			}
		}
	}
	sk.AllowedTools, _ = str("allowed-tools")
	if sk.License == "" {
		sk.Warnings = append(sk.Warnings, "no license field")
	}
	if len(body) > 64<<10 {
		sk.Warnings = append(sk.Warnings, "SKILL.md body is over 64 KiB; consider moving detail into references/")
	}

	files, scripts, size, digest, treeProblems := walk(abs, lim)
	problems = append(problems, treeProblems...)
	sk.Files, sk.Scripts, sk.Size, sk.Digest = files, scripts, size, digest
	if len(problems) > 0 {
		return sk, &ValidationError{Dir: dir, Problems: problems}
	}
	return sk, nil
}

func splitFrontmatter(b []byte) (front []byte, body string, err error) {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", errors.New("SKILL.md must start with YAML frontmatter between --- lines")
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, "", errors.New("SKILL.md frontmatter is not closed with ---")
	}
	after := rest[end+4:]
	if after != "" && !strings.HasPrefix(after, "\n") {
		return nil, "", errors.New("SKILL.md frontmatter closing line must be exactly ---")
	}
	return []byte(rest[:end]), strings.TrimPrefix(after, "\n"), nil
}

// walk lists files, rejects escapes and computes the digest.
//
// The digest is SHA-256 over lines "<kind> <mode> <sha256> <path>\n" for
// every entry in sorted path order, where kind is f (file) or l (symlink, hashed
// over its target string). Modification times are ignored.
func walk(root string, lim Limits) (files, scripts []string, size int64, digest string, problems []string) {
	type entry struct{ line, rel string }
	var entries []entry
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			if err != nil {
				problems = append(problems, rel+": unreadable symlink")
				return nil
			}
			if filepath.IsAbs(target) {
				problems = append(problems, rel+": symlink with an absolute target")
				return nil
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(p), target))
			if r, err := filepath.Rel(root, resolved); err != nil || r == ".." || strings.HasPrefix(r, "../") {
				problems = append(problems, rel+": symlink points outside the skill")
				return nil
			}
			sum := sha256.Sum256([]byte(target))
			entries = append(entries, entry{fmt.Sprintf("l 0 %x %s\n", sum, rel), rel})
			return nil
		}
		if !info.Mode().IsRegular() {
			problems = append(problems, rel+": not a regular file")
			return nil
		}
		if info.Size() > lim.MaxFileBytes {
			problems = append(problems, fmt.Sprintf("%s: larger than %d bytes", rel, lim.MaxFileBytes))
			return nil
		}
		size += info.Size()
		if len(entries) >= lim.MaxFiles {
			problems = append(problems, fmt.Sprintf("more than %d files", lim.MaxFiles))
			return filepath.SkipAll
		}
		f, err := os.Open(p)
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			problems = append(problems, err.Error())
			return nil
		}
		exec := info.Mode().Perm()&0o111 != 0
		mode := 0
		if exec {
			mode = 1
		}
		entries = append(entries, entry{fmt.Sprintf("f %d %x %s\n", mode, h.Sum(nil), rel), rel})
		files = append(files, rel)
		if exec || strings.HasPrefix(rel, "scripts/") {
			scripts = append(scripts, rel)
		}
		return nil
	})
	if err != nil {
		problems = append(problems, err.Error())
	}
	if size > lim.MaxTotalBytes {
		problems = append(problems, fmt.Sprintf("skill is larger than %d bytes", lim.MaxTotalBytes))
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(a.rel, b.rel) })
	h := sha256.New()
	for _, e := range entries {
		io.WriteString(h, e.line)
	}
	slices.Sort(files)
	slices.Sort(scripts)
	return files, scripts, size, "sha256:" + hex.EncodeToString(h.Sum(nil)), problems
}

// Digest recomputes only the digest of dir.
func Digest(dir string, lim Limits) (string, error) {
	_, _, _, d, problems := walk(dir, lim)
	if len(problems) > 0 {
		return "", &ValidationError{Dir: dir, Problems: problems}
	}
	return d, nil
}
