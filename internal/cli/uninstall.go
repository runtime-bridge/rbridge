package cli

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
)

type uninstallItem struct {
	Kind   string `json:"kind"` // program, configuration, data, cache or runtime
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Bytes  int64  `json:"bytes,omitempty"`
}

type uninstallPlan struct {
	// Bundled names the package manager that owns the program, when rbridge
	// runs from inside a Python or Node.js package.
	Bundled string          `json:"bundled,omitempty"`
	Items   []uninstallItem `json:"items"`
	Removed []string        `json:"removed,omitempty"`
}

// cmdUninstall shows where this rbridge and its data are, and removes them
// when asked: the program with --yes, the data folders too with --purge.
func cmdUninstall(_ context.Context, args []string, env Env) error {
	flags := newFlags("uninstall", env)
	yes := flags.Bool("yes", false, "remove what is listed instead of only listing it")
	purge := flags.Bool("purge", false, "also remove the configuration, skills, sessions and audit log")
	asJSON := flags.Bool("json", false, "print JSON")
	pos, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge uninstall [--purge] [--yes]")
	}
	paths, err := config.ResolvePaths(config.Env{GOOS: env.GOOS, Getenv: env.Getenv, HomeDir: env.Home, UID: env.UID})
	if err != nil {
		return domain.Errorf(domain.ErrInvalidInput, "%v", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	plan := uninstallPlan{Bundled: env.Getenv("RBRIDGE_BUNDLED")}
	plan.Items = append(plan.Items, measure("program", exe))
	for _, d := range []struct{ kind, path string }{
		{"configuration", paths.ConfigDir}, {"data", paths.DataDir}, {"cache", paths.CacheDir}, {"runtime", paths.RuntimeDir},
	} {
		plan.Items = append(plan.Items, measure(d.kind, d.path))
	}
	home := env.Getenv("RBRIDGE_HOME")

	if *yes {
		if f, err := daemonFiles(env); err == nil && alive(f) {
			if err := daemonStop(env); err != nil {
				return err
			}
		}
		if *purge {
			for _, it := range plan.Items[1:] {
				if !it.Exists {
					continue
				}
				if err := safeToRemove(it.Path, env.Home); err != nil {
					return err
				}
				if err := os.RemoveAll(it.Path); err != nil {
					return err
				}
				plan.Removed = append(plan.Removed, it.Path)
			}
			if home != "" {
				// The home itself goes too once it holds nothing else.
				if entries, err := os.ReadDir(home); err == nil && len(entries) == 0 {
					if os.Remove(home) == nil {
						plan.Removed = append(plan.Removed, home)
					}
				}
			}
		}
		if plan.Bundled == "" && runtime.GOOS != "windows" {
			if err := os.Remove(exe); err != nil {
				return fmt.Errorf("remove %s: %w", exe, err)
			}
			plan.Removed = append(plan.Removed, exe)
		}
	}

	if *asJSON {
		return writeJSON(env.Stdout, plan)
	}
	for _, it := range plan.Items {
		state := "not present"
		if it.Exists {
			state = humanBytes(it.Bytes)
		}
		mark := " "
		if *yes && contains(plan.Removed, it.Path) {
			mark = "x"
		}
		fmt.Fprintf(env.Stdout, "[%s] %-13s %s (%s)\n", mark, it.Kind, it.Path, state)
	}
	switch {
	case plan.Bundled != "":
		manager, where, _ := strings.Cut(plan.Bundled, ":")
		fmt.Fprintf(env.Stdout, "\nThis rbridge came with the %s SDK in %s, so its package manager owns the program:\n", map[string]string{"python": "Python", "node": "Node.js"}[manager], where)
		if manager == "python" {
			fmt.Fprintln(env.Stdout, "  pip uninstall rbridge          (or delete the virtual environment)")
		} else {
			fmt.Fprintln(env.Stdout, "  npm uninstall @runtime-bridge/sdk   (or delete node_modules)")
		}
	case *yes && runtime.GOOS == "windows":
		fmt.Fprintf(env.Stdout, "\nWindows cannot delete a running program; delete %s after this command ends.\n", exe)
	}
	switch {
	case !*yes && plan.Bundled != "":
		fmt.Fprintln(env.Stdout, "\nNothing was changed. rbridge uninstall --purge --yes removes the configuration, skills, sessions and audit log.")
	case !*yes:
		fmt.Fprintln(env.Stdout, "\nNothing was changed. rbridge uninstall --yes removes the program; add --purge to remove the configuration, skills, sessions and audit log too.")
	case !*purge:
		fmt.Fprintln(env.Stdout, "\nThe configuration, skills, sessions and audit log were kept; rbridge uninstall --purge --yes removes them.")
	}
	return nil
}

func measure(kind, path string) uninstallItem {
	it := uninstallItem{Kind: kind, Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return it
	}
	it.Exists = true
	if !info.IsDir() {
		it.Bytes = info.Size()
		return it
	}
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if i, err := d.Info(); err == nil {
				it.Bytes += i.Size()
			}
		}
		return nil
	})
	return it
}

// safeToRemove refuses folders that cannot be rbridge's own: the file system
// root, the user's home and anything above it.
func safeToRemove(path, home string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == filepath.Dir(clean) {
		return domain.Errorf(domain.ErrInvalidInput, "refusing to remove %s", path)
	}
	if home != "" {
		h := filepath.Clean(home)
		if clean == h || strings.HasPrefix(h, clean+string(filepath.Separator)) {
			return domain.Errorf(domain.ErrInvalidInput, "refusing to remove %s: it holds the home folder", path)
		}
	}
	return nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
