// Package config resolves rbridge's directories and loads layered configuration.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
)

const appName = "rbridge"

// ProjectFileName is the optional per-project configuration file.
const ProjectFileName = ".rbridge.yaml"

// Paths are the directories rbridge reads and writes.
type Paths struct {
	SystemConfig string // system configuration file
	ConfigDir    string // user configuration directory
	DataDir      string // skills, session index, audit
	CacheDir     string // detection cache
	RuntimeDir   string // sockets and other per-login state
}

// UserConfigFile is the user-level configuration file.
func (p Paths) UserConfigFile() string { return filepath.Join(p.ConfigDir, "config.yaml") }

// Env is the part of the process environment that path resolution depends on.
type Env struct {
	GOOS    string
	Getenv  func(string) string
	HomeDir string
	UID     int
}

// OSEnv returns the environment of the running process.
func OSEnv() (Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Env{}, fmt.Errorf("find home directory: %w", err)
	}
	return Env{GOOS: runtime.GOOS, Getenv: os.Getenv, HomeDir: home, UID: os.Getuid()}, nil
}

// ResolvePaths applies RBRIDGE_HOME when set, otherwise the OS conventions.
func ResolvePaths(env Env) (Paths, error) {
	if env.Getenv == nil {
		return Paths{}, errors.New("config: Env.Getenv is nil")
	}
	if h := env.Getenv("RBRIDGE_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return Paths{}, fmt.Errorf("RBRIDGE_HOME must be an absolute path: %q", h)
		}
		return Paths{
			SystemConfig: filepath.Join(h, "system", "config.yaml"),
			ConfigDir:    filepath.Join(h, "config"),
			DataDir:      filepath.Join(h, "data"),
			CacheDir:     filepath.Join(h, "cache"),
			RuntimeDir:   filepath.Join(h, "run"),
		}, nil
	}
	if env.HomeDir == "" {
		return Paths{}, errors.New("config: home directory is unknown")
	}
	home := env.HomeDir
	switch env.GOOS {
	case "windows":
		appData := orDefault(env.Getenv("AppData"), filepath.Join(home, "AppData", "Roaming"))
		local := orDefault(env.Getenv("LocalAppData"), filepath.Join(home, "AppData", "Local"))
		programData := orDefault(env.Getenv("ProgramData"), `C:\ProgramData`)
		return Paths{
			SystemConfig: filepath.Join(programData, appName, "config.yaml"),
			ConfigDir:    filepath.Join(appData, appName),
			DataDir:      filepath.Join(local, appName),
			CacheDir:     filepath.Join(local, appName, "cache"),
			RuntimeDir:   filepath.Join(local, appName, "run"),
		}, nil
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support", appName)
		tmp := orDefault(env.Getenv("TMPDIR"), "/tmp")
		return Paths{
			SystemConfig: filepath.Join("/Library", "Application Support", appName, "config.yaml"),
			ConfigDir:    support,
			DataDir:      filepath.Join(support, "data"),
			CacheDir:     filepath.Join(home, "Library", "Caches", appName),
			RuntimeDir:   filepath.Join(tmp, appName+"-"+strconv.Itoa(env.UID)),
		}, nil
	default:
		data := filepath.Join(xdg(env, "XDG_DATA_HOME", filepath.Join(home, ".local", "share")), appName)
		runtimeDir := filepath.Join(data, "run")
		if r := env.Getenv("XDG_RUNTIME_DIR"); r != "" && filepath.IsAbs(r) {
			runtimeDir = filepath.Join(r, appName)
		}
		return Paths{
			SystemConfig: filepath.Join("/etc", appName, "config.yaml"),
			ConfigDir:    filepath.Join(xdg(env, "XDG_CONFIG_HOME", filepath.Join(home, ".config")), appName),
			DataDir:      data,
			CacheDir:     filepath.Join(xdg(env, "XDG_CACHE_HOME", filepath.Join(home, ".cache")), appName),
			RuntimeDir:   runtimeDir,
		}, nil
	}
}

// xdg returns an XDG base directory, ignoring relative values as the spec requires.
func xdg(env Env, name, fallback string) string {
	if v := env.Getenv(name); v != "" && filepath.IsAbs(v) {
		return v
	}
	return fallback
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
