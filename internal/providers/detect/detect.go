// Package detect finds installed runtimes without starting an agent session.
//
// It only looks at executable files and runs each runtime's documented
// version command. It never opens configuration or credential files.
package detect

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/providers/registry"
)

// FS is the filesystem access detection needs. Tests replace it to record
// every path that is touched.
type FS interface {
	Stat(path string) (fs.FileInfo, error)
	EvalSymlinks(path string) (string, error)
}

type osFS struct{}

func (osFS) Stat(p string) (fs.FileInfo, error)    { return os.Stat(p) }
func (osFS) EvalSymlinks(p string) (string, error) { return filepath.EvalSymlinks(p) }

// OSFS is the real filesystem.
var OSFS FS = osFS{}

// RunFunc runs a version command. It matches process.Run.
type RunFunc func(ctx context.Context, s process.Spec) (process.Output, error)

// Detector finds runtimes for manifests.
type Detector struct {
	FS      FS
	Run     RunFunc
	Getenv  func(string) string
	Environ func() []string
	GOOS    string
	Home    string
	// EnvAllowlist is passed to version commands, plus each manifest's passthrough names.
	EnvAllowlist []string
	Timeout      time.Duration
	Now          func() time.Time
}

// Detect inspects one provider. It never returns an error: problems are
// reported through Health and Installation.Error.
func (d *Detector) Detect(ctx context.Context, m registry.Manifest, entry config.ProviderEntry) domain.RuntimeInfo {
	info := domain.RuntimeInfo{
		ProviderID:  m.ID,
		Integration: m.Integration,
		AuthMode:    d.authMode(m),
		DetectedAt:  d.now(),
	}
	if !entry.IsEnabled() {
		info.Health = domain.HealthDisabled
		return info
	}

	var candidates []string
	if entry.Executable != "" {
		info.Pinned = true
		candidates = []string{entry.Executable}
	} else {
		candidates = d.search(m)
	}

	for _, p := range candidates {
		inst := domain.Installation{Path: p}
		if !d.isExecutable(p) {
			inst.Error = "not an executable file"
			info.Installations = append(info.Installations, inst)
			continue
		}
		if len(m.Detect.VersionArgs) > 0 {
			v, err := d.version(ctx, m, p)
			inst.Version = v
			if err != "" {
				inst.Error = err
			}
		}
		info.Installations = append(info.Installations, inst)
	}

	for _, inst := range info.Installations {
		if inst.Error == "" {
			info.Executable = inst.Path
			info.Version = inst.Version
			info.Health = domain.HealthAvailable
			if len(m.Detect.VersionArgs) == 0 {
				info.Notes = append(info.Notes, "version not checked: no version command configured")
			}
			break
		}
	}
	if info.Health == domain.HealthAvailable && m.RequireAuthEnv && info.AuthMode != domain.AuthAPIKey && (m.AccountLoginForbidden || !entry.AllowAccountLogin) {
		info.Health = domain.HealthNeedsAPIKey
		if m.AccountLoginForbidden {
			info.Notes = append(info.Notes, "set one of "+strings.Join(m.AuthEnv, ", ")+"; this provider cannot use an account login")
		} else {
			info.Notes = append(info.Notes, "set one of "+strings.Join(m.AuthEnv, ", ")+", or providers."+m.ID+".allow_account_login: true after reading the provider's terms")
		}
	}
	if info.Health == domain.HealthAvailable && entry.AllowAccountLogin && info.AuthMode == domain.AuthUnknown && m.RequireAuthEnv {
		info.AuthMode = domain.AuthSubscription
	}
	switch {
	case info.Health == domain.HealthAvailable, info.Health == domain.HealthNeedsAPIKey:
	case len(info.Installations) > 0:
		info.Health = domain.HealthUnhealthy
		info.Executable = info.Installations[0].Path
	default:
		info.Health = domain.HealthMissing
	}
	if len(info.Installations) > 1 && !info.Pinned {
		info.Notes = append(info.Notes, "several installations found; pin one with providers."+m.ID+".executable")
	}
	return info
}

func (d *Detector) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// authMode looks only at the names of environment variables.
func (d *Detector) authMode(m registry.Manifest) domain.AuthMode {
	for _, name := range m.AuthEnv {
		if d.Getenv(name) != "" {
			return domain.AuthAPIKey
		}
	}
	return domain.AuthUnknown
}

// search returns candidate executables from PATH, then known directories,
// without duplicates (compared after resolving symlinks).
func (d *Detector) search(m registry.Manifest) []string {
	var dirs []string
	for _, dir := range filepath.SplitList(d.Getenv("PATH")) {
		if dir != "" && filepath.IsAbs(dir) {
			dirs = append(dirs, dir)
		}
	}
	for _, k := range m.Detect.KnownDirs {
		if dir := d.Expand(k); dir != "" {
			dirs = append(dirs, dir)
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range m.Detect.Executables {
		if filepath.IsAbs(name) {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			continue
		}
		for _, dir := range dirs {
			for _, file := range d.names(name) {
				p := filepath.Join(dir, file)
				if _, err := d.FS.Stat(p); err != nil {
					continue
				}
				key := p
				if r, err := d.FS.EvalSymlinks(p); err == nil {
					key = r
				}
				if seen[key] {
					continue
				}
				seen[key] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// names adds Windows executable extensions.
func (d *Detector) names(name string) []string {
	if d.GOOS != "windows" || filepath.Ext(name) != "" {
		return []string{name}
	}
	exts := d.Getenv("PATHEXT")
	if exts == "" {
		exts = ".COM;.EXE;.BAT;.CMD"
	}
	var out []string
	for e := range strings.SplitSeq(exts, ";") {
		if e != "" {
			out = append(out, name+strings.ToLower(e))
		}
	}
	return out
}

func (d *Detector) isExecutable(p string) bool {
	fi, err := d.FS.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}
	if d.GOOS == "windows" {
		return true
	}
	return fi.Mode().Perm()&0o111 != 0
}

var versionPattern = regexp.MustCompile(`\d+\.\d+(\.\d+)?([-+.][0-9A-Za-z.-]+)?`)

// version runs the version command and returns the version or an error text.
func (d *Detector) version(ctx context.Context, m registry.Manifest, path string) (string, string) {
	allow := append(append([]string(nil), d.EnvAllowlist...), m.Launch.EnvPassthrough...)
	env := process.FilterEnv(d.Environ(), allow, nil)
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	out, err := d.Run(ctx, process.Spec{
		Path:      path,
		Args:      m.Detect.VersionArgs,
		Env:       env,
		Dir:       d.Home,
		Timeout:   timeout,
		MaxOutput: 64 << 10,
		Grace:     time.Second,
	})
	switch {
	case errors.Is(err, process.ErrTimeout):
		return "", "version command timed out"
	case err != nil:
		return "", "version command failed to start: " + err.Error()
	case out.ExitCode != 0:
		return "", "version command exited with code " + strconv.Itoa(out.ExitCode)
	}
	text := string(out.Stdout)
	if strings.TrimSpace(text) == "" {
		text = string(out.Stderr)
	}
	if v := versionPattern.FindString(text); v != "" {
		return v, ""
	}
	return "", "version output not recognised"
}

// Expand resolves a leading ~ and $VAR in a manifest path. It returns ""
// when a variable is unset or the result is not absolute.
func (d *Detector) Expand(p string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if d.Home == "" {
			return ""
		}
		p = filepath.Join(d.Home, strings.TrimPrefix(p, "~"))
	case strings.HasPrefix(p, "$"):
		name, rest, _ := strings.Cut(p[1:], "/")
		v := d.Getenv(name)
		if v == "" {
			return ""
		}
		p = filepath.Join(v, filepath.FromSlash(rest))
	}
	p = filepath.FromSlash(p)
	if !filepath.IsAbs(p) {
		return ""
	}
	return filepath.Clean(p)
}
