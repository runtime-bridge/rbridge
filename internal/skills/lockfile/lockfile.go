// Package lockfile reads skills-lock.json, the project lock file written by
// the skills installer (github.com/vercel-labs/skills), and computes its
// content hash so an installed copy can be checked against the lock.
package lockfile

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Name is the lock file's name in a project.
const Name = "skills-lock.json"

// Entry is one skill in the lock. Fields rbridge does not use are ignored.
type Entry struct {
	Source       string `json:"source"`
	SourceURL    string `json:"sourceUrl,omitempty"`
	Ref          string `json:"ref,omitempty"`
	Commit       string `json:"commit,omitempty"`
	SourceType   string `json:"sourceType"`
	SkillPath    string `json:"skillPath,omitempty"`
	SkillName    string `json:"skillName,omitempty"`
	ComputedHash string `json:"computedHash"`
}

// Lock is a parsed skills-lock.json.
type Lock struct {
	Version int              `json:"version"`
	Skills  map[string]Entry `json:"skills"`
}

// Read parses the lock file at path. Version 1 locks are read, but their
// hashes use an older algorithm and are dropped, so their entries cannot be
// verified.
func Read(path string) (Lock, error) {
	var l Lock
	b, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	if bytes.Contains(b, []byte("<<<<<<<")) {
		return l, errors.New("the lock file has merge conflict markers")
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return l, fmt.Errorf("the lock file is not valid JSON: %w", err)
	}
	if l.Skills == nil {
		return l, errors.New(`the lock file has no "skills" map`)
	}
	switch l.Version {
	case 2:
	case 1:
		for k, e := range l.Skills {
			e.ComputedHash = ""
			l.Skills[k] = e
		}
	default:
		return l, fmt.Errorf("unsupported lock version %d", l.Version)
	}
	return l, nil
}

// GitURL returns the repository URL and the skill's folder inside it, when
// the entry is a Git source pinned to a commit.
func (e Entry) GitURL() (url, subdir string, ok bool) {
	if e.Commit == "" {
		return "", "", false
	}
	switch {
	case e.SourceURL != "" && (strings.HasPrefix(e.SourceURL, "https://") || strings.HasPrefix(e.SourceURL, "ssh://") || strings.HasPrefix(e.SourceURL, "git@") || strings.HasPrefix(e.SourceURL, "file://")):
		url = e.SourceURL
	case e.SourceType == "github" && strings.Count(e.Source, "/") == 1 && !strings.ContainsAny(e.Source, " :\\"):
		url = "https://github.com/" + e.Source + ".git"
	case e.SourceType == "git" && strings.HasPrefix(e.Source, "https://"):
		url = e.Source
	default:
		return "", "", false
	}
	if e.SkillPath != "" {
		subdir = strings.TrimSuffix(filepath.ToSlash(filepath.Dir(filepath.FromSlash(e.SkillPath))), "/")
		if subdir == "." {
			subdir = ""
		}
	}
	return url, subdir, true
}

var excludedFiles = map[string]bool{"metadata.json": true}
var excludedDirs = map[string]bool{".git": true, "__pycache__": true, "__pypackages__": true}

type hashedFile struct {
	path    []byte
	content []byte
}

// Hash computes the lock's computedHash (version 2) for a skill folder:
// files sorted by the UTF-8 bytes of their slash-separated paths, each
// framed as "path NUL length NUL content", with CRLF turned into LF in
// files without NUL bytes. Symbolic links count as their target when it
// stays inside the folder and are skipped otherwise.
func Hash(dir string) (string, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	var files []hashedFile
	if err := collect(dir, "", root, map[string]bool{}, &files); err != nil {
		return "", err
	}
	sort.Slice(files, func(i, j int) bool { return bytes.Compare(files[i].path, files[j].path) < 0 })
	h := sha256.New()
	for _, f := range files {
		h.Write(f.path)
		h.Write([]byte{0})
		h.Write([]byte(strconv.Itoa(len(f.content))))
		h.Write([]byte{0})
		h.Write(f.content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func collect(dir, prefix, root string, ancestors map[string]bool, out *[]hashedFile) error {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if ancestors[real] {
		return nil
	}
	branch := map[string]bool{real: true}
	for k := range ancestors {
		branch[k] = true
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		isDir := e.IsDir()
		if (isDir && excludedDirs[name]) || (!isDir && excludedFiles[name]) {
			continue
		}
		full := filepath.Join(dir, name)
		if e.Type()&fs.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(full)
			if err != nil || (target != root && !strings.HasPrefix(target, root+string(filepath.Separator))) {
				continue
			}
			fi, err := os.Stat(target)
			if err != nil {
				continue
			}
			isDir = fi.IsDir()
			if isDir && excludedDirs[name] {
				continue
			}
			if !isDir && excludedFiles[name] {
				continue
			}
		} else if !isDir && !e.Type().IsRegular() {
			continue
		}
		if isDir {
			if err := collect(full, prefix+name+"/", root, branch, out); err != nil {
				return err
			}
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		if !bytes.Contains(b, []byte{0}) && bytes.Contains(b, []byte("\r\n")) {
			b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
		}
		*out = append(*out, hashedFile{path: []byte(prefix + name), content: b})
	}
	return nil
}
