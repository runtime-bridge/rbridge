// Package accounts finds the separate Claude Code accounts on a machine.
//
// Claude Code keeps each account in its own configuration directory, chosen
// with CLAUDE_CONFIG_DIR (default ~/.claude). People who use several
// accounts usually have a small wrapper script per account (claude-work,
// claude-pro, ...) that sets CLAUDE_CONFIG_DIR and runs claude, and a
// ~/.claude-<name> directory for each.
//
// Discovery only lists directories and reads wrapper scripts looking for the
// CLAUDE_CONFIG_DIR assignment. It never opens anything inside a
// configuration directory and never reads files a wrapper sources.
package accounts

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Account is one Claude Code configuration directory.
type Account struct {
	Name      string `json:"name"`
	ConfigDir string `json:"config_dir"`
	// Launcher is the wrapper script that starts this account, if any. When
	// set, rbridge runs it instead of claude so the wrapper's own settings apply.
	Launcher string `json:"launcher,omitempty"`
	// Source is "wrapper", "directory" or "config".
	Source string `json:"source"`
}

// FS is the filesystem access discovery needs.
type FS interface {
	ReadDir(name string) ([]fs.DirEntry, error)
	Stat(name string) (fs.FileInfo, error)
	ReadFile(name string) ([]byte, error)
}

type osFS struct{}

func (osFS) ReadDir(n string) ([]fs.DirEntry, error) { return os.ReadDir(n) }
func (osFS) Stat(n string) (fs.FileInfo, error)      { return os.Stat(n) }
func (osFS) ReadFile(n string) ([]byte, error)       { return os.ReadFile(n) }

// OSFS is the real filesystem.
var OSFS FS = osFS{}

// Options say where to look.
type Options struct {
	FS   FS
	Home string
	// BinDirs are searched for claude-<name> wrapper scripts, usually PATH.
	BinDirs []string
	// Wrappers and Dirs turn the two discovery methods on.
	Wrappers bool
	Dirs     bool
}

var (
	namePattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	assignPattern = regexp.MustCompile(`(?m)^\s*(?:export\s+)?CLAUDE_CONFIG_DIR=(?:"([^"]+)"|'([^']+)'|([^\s#;]+))`)
)

// ValidName reports whether n can be used in a provider id.
func ValidName(n string) bool { return len(n) <= 40 && namePattern.MatchString(n) }

// Default returns the default configuration directory, ~/.claude.
func Default(home string) string { return filepath.Join(home, ".claude") }

// Discover returns the accounts other than the default one, sorted by name.
// A wrapper and a directory for the same configuration directory become one
// account that is started through the wrapper.
func Discover(o Options) []Account {
	if o.FS == nil {
		o.FS = OSFS
	}
	byDir := map[string]Account{}
	add := func(a Account) {
		a.ConfigDir = filepath.Clean(a.ConfigDir)
		if a.ConfigDir == filepath.Clean(Default(o.Home)) || !ValidName(a.Name) {
			return
		}
		if old, ok := byDir[a.ConfigDir]; ok && old.Launcher != "" {
			return
		}
		byDir[a.ConfigDir] = a
	}
	if o.Wrappers {
		seen := map[string]bool{}
		for _, dir := range o.BinDirs {
			entries, err := o.FS.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				name, ok := strings.CutPrefix(e.Name(), "claude-")
				if !ok || e.IsDir() || seen[e.Name()] {
					continue
				}
				path := filepath.Join(dir, e.Name())
				cfg, ok := wrapperConfigDir(o.FS, path, o.Home)
				if !ok {
					continue
				}
				seen[e.Name()] = true
				add(Account{Name: strings.ToLower(name), ConfigDir: cfg, Launcher: path, Source: "wrapper"})
			}
		}
	}
	if o.Dirs && o.Home != "" {
		entries, _ := o.FS.ReadDir(o.Home)
		for _, e := range entries {
			name, ok := strings.CutPrefix(e.Name(), ".claude-")
			if !ok || !e.IsDir() {
				continue
			}
			add(Account{Name: strings.ToLower(name), ConfigDir: filepath.Join(o.Home, e.Name()), Source: "directory"})
		}
	}
	// Two configuration directories must not end up with the same name.
	out := make([]Account, 0, len(byDir))
	names := map[string]bool{}
	for _, a := range byDir {
		out = append(out, a)
	}
	slices.SortFunc(out, func(a, b Account) int {
		if a.Launcher != "" && b.Launcher == "" {
			return -1
		}
		if a.Launcher == "" && b.Launcher != "" {
			return 1
		}
		return strings.Compare(a.ConfigDir, b.ConfigDir)
	})
	var unique []Account
	for _, a := range out {
		if names[a.Name] {
			continue
		}
		names[a.Name] = true
		unique = append(unique, a)
	}
	slices.SortFunc(unique, func(a, b Account) int { return strings.Compare(a.Name, b.Name) })
	return unique
}

// wrapperConfigDir reads a small executable script and returns the
// CLAUDE_CONFIG_DIR it sets.
func wrapperConfigDir(f FS, path, home string) (string, bool) {
	fi, err := f.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > 64<<10 || fi.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	b, err := f.ReadFile(path)
	if err != nil || !strings.HasPrefix(string(b), "#!") {
		return "", false
	}
	m := assignPattern.FindSubmatch(b)
	if m == nil {
		return "", false
	}
	v := string(m[1]) + string(m[2]) + string(m[3])
	return expandHome(v, home)
}

func expandHome(v, home string) (string, bool) {
	for _, prefix := range []string{"$HOME", "${HOME}", "~"} {
		if rest, ok := strings.CutPrefix(v, prefix); ok {
			if home == "" {
				return "", false
			}
			v = home + rest
			break
		}
	}
	if strings.ContainsAny(v, "$`") || !filepath.IsAbs(v) {
		return "", false
	}
	return filepath.Clean(v), true
}

// Expand resolves ~ and $HOME in a configured directory.
func Expand(v, home string) (string, bool) { return expandHome(v, home) }
