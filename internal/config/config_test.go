package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func testPaths(t *testing.T) Paths {
	t.Helper()
	h := t.TempDir()
	p, err := ResolvePaths(Env{GOOS: "linux", Getenv: func(k string) string {
		if k == "RBRIDGE_HOME" {
			return h
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDefaultsOnly(t *testing.T) {
	cfg, layers, err := Load(testPaths(t), LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Execution.TaskTimeout != 30*time.Minute {
		t.Errorf("task timeout = %v", cfg.Execution.TaskTimeout)
	}
	if !slices.Equal(cfg.Providers.Order, []string{"claude-code", "opencode"}) {
		t.Errorf("order = %v", cfg.Providers.Order)
	}
	if len(layers) != 3 || layers[1].Found || layers[2].Found {
		t.Errorf("unexpected layers %+v", layers)
	}
}

func TestLayerPrecedence(t *testing.T) {
	p := testPaths(t)
	proj := t.TempDir()
	write(t, p.SystemConfig, "execution:\n  task_timeout: 20m\n  max_concurrent_sessions: 3\n")
	write(t, p.UserConfigFile(), "execution:\n  task_timeout: 15m\nproviders:\n  order: [opencode]\n  opencode:\n    enabled: false\n")
	write(t, filepath.Join(proj, ProjectFileName), "execution:\n  task_timeout: 5m\n")

	cfg, _, err := Load(p, LoadOptions{ProjectDir: proj, Task: map[string]any{
		"execution": map[string]any{"command_timeout": "1m"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Execution.TaskTimeout != 5*time.Minute {
		t.Errorf("project layer should win: %v", cfg.Execution.TaskTimeout)
	}
	if cfg.Execution.MaxConcurrentSessions != 3 {
		t.Errorf("system value should survive: %d", cfg.Execution.MaxConcurrentSessions)
	}
	if cfg.Execution.CommandTimeout != time.Minute {
		t.Errorf("task layer should apply: %v", cfg.Execution.CommandTimeout)
	}
	if !slices.Equal(cfg.Providers.Order, []string{"opencode"}) {
		t.Errorf("lists replace, got %v", cfg.Providers.Order)
	}
	if cfg.Providers.Settings["opencode"].IsEnabled() {
		t.Error("opencode should be disabled")
	}
	if !cfg.Providers.Settings["claude-code"].IsEnabled() {
		t.Error("providers are enabled by default")
	}
}

func TestSystemLocks(t *testing.T) {
	p := testPaths(t)
	write(t, p.SystemConfig, "locked: [policy.default_profile, providers.opencode.enabled]\npolicy:\n  default_profile: observe\nproviders:\n  opencode:\n    enabled: false\n")
	write(t, p.UserConfigFile(), "locked: []\npolicy:\n  default_profile: standard\nproviders:\n  opencode:\n    enabled: true\n")
	cfg, layers, err := Load(p, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Policy.DefaultProfile != "observe" {
		t.Errorf("locked key changed: %s", cfg.Policy.DefaultProfile)
	}
	if cfg.Providers.Settings["opencode"].IsEnabled() {
		t.Error("locked provider setting changed")
	}
	if len(layers[2].Ignored) != 2 {
		t.Errorf("user layer should report two ignored keys, got %v", layers[2].Ignored)
	}
	if len(cfg.Locked) != 2 {
		t.Errorf("locked = %v", cfg.Locked)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	p := testPaths(t)
	write(t, p.UserConfigFile(), "execution:\n  task_timout: 5m\n")
	_, _, err := Load(p, LoadOptions{})
	if err == nil || !strings.Contains(err.Error(), "task_timout") {
		t.Fatalf("want unknown field error, got %v", err)
	}
}

func TestValidation(t *testing.T) {
	cases := map[string]string{
		"bad profile":    "policy:\n  default_profile: yolo\n",
		"relative pin":   "providers:\n  claude-code:\n    executable: bin/claude\n",
		"bad id":         "providers:\n  order: [Claude_Code]\n",
		"custom no cmd":  "providers:\n  custom:\n    - id: mine\n",
		"custom dup":     "providers:\n  custom:\n    - {id: a, launch: {command: /x}}\n    - {id: a, launch: {command: /y}}\n",
		"zero timeout":   "execution:\n  task_timeout: 0s\n",
		"abs skills dir": "skills:\n  project_dir: /abs\n",
		"bad fallback":   "resolution:\n  fallback: maybe\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := testPaths(t)
			write(t, p.UserConfigFile(), body)
			if _, _, err := Load(p, LoadOptions{}); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestEmptyFileIsValid(t *testing.T) {
	p := testPaths(t)
	write(t, p.UserConfigFile(), "\n")
	if _, _, err := Load(p, LoadOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestCustomProvider(t *testing.T) {
	p := testPaths(t)
	write(t, p.UserConfigFile(), "providers:\n  custom:\n    - id: my-agent\n      launch:\n        command: /usr/local/bin/my-agent\n        args: [--acp]\n")
	cfg, _, err := Load(p, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers.Custom) != 1 || cfg.Providers.Custom[0].Launch.Args[0] != "--acp" {
		t.Fatalf("custom = %+v", cfg.Providers.Custom)
	}
}

func TestResolvePaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("expectations use POSIX paths; Windows paths are covered by TestResolvePathsWindows")
	}
	env := func(goos string, vars map[string]string) Env {
		return Env{GOOS: goos, HomeDir: "/home/u", UID: 1000, Getenv: func(k string) string { return vars[k] }}
	}
	cases := []struct {
		name string
		env  Env
		want Paths
	}{
		{"linux defaults", env("linux", nil), Paths{
			SystemConfig: "/etc/rbridge/config.yaml",
			ConfigDir:    "/home/u/.config/rbridge",
			DataDir:      "/home/u/.local/share/rbridge",
			CacheDir:     "/home/u/.cache/rbridge",
			RuntimeDir:   "/home/u/.local/share/rbridge/run",
		}},
		{"linux xdg", env("linux", map[string]string{"XDG_CONFIG_HOME": "/x/c", "XDG_DATA_HOME": "/x/d", "XDG_CACHE_HOME": "relative", "XDG_RUNTIME_DIR": "/run/user/1000"}), Paths{
			SystemConfig: "/etc/rbridge/config.yaml",
			ConfigDir:    "/x/c/rbridge",
			DataDir:      "/x/d/rbridge",
			CacheDir:     "/home/u/.cache/rbridge",
			RuntimeDir:   "/run/user/1000/rbridge",
		}},
		{"darwin", env("darwin", map[string]string{"TMPDIR": "/tmp/abc"}), Paths{
			SystemConfig: "/Library/Application Support/rbridge/config.yaml",
			ConfigDir:    "/home/u/Library/Application Support/rbridge",
			DataDir:      "/home/u/Library/Application Support/rbridge/data",
			CacheDir:     "/home/u/Library/Caches/rbridge",
			RuntimeDir:   "/tmp/abc/rbridge-1000",
		}},
		{"home override", env("linux", map[string]string{"RBRIDGE_HOME": "/r"}), Paths{
			SystemConfig: "/r/system/config.yaml",
			ConfigDir:    "/r/config",
			DataDir:      "/r/data",
			CacheDir:     "/r/cache",
			RuntimeDir:   "/r/run",
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolvePaths(c.env)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("got %+v\nwant %+v", got, c.want)
			}
		})
	}
	if _, err := ResolvePaths(env("linux", map[string]string{"RBRIDGE_HOME": "rel"})); err == nil {
		t.Error("relative RBRIDGE_HOME should fail")
	}
}

func TestResolvePathsWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("filepath joins with the host separator")
	}
	vars := map[string]string{"AppData": `C:\Users\u\AppData\Roaming`, "LocalAppData": `C:\Users\u\AppData\Local`, "ProgramData": `C:\ProgramData`}
	got, err := ResolvePaths(Env{GOOS: "windows", HomeDir: `C:\Users\u`, Getenv: func(k string) string { return vars[k] }})
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		SystemConfig: `C:\ProgramData\rbridge\config.yaml`,
		ConfigDir:    `C:\Users\u\AppData\Roaming\rbridge`,
		DataDir:      `C:\Users\u\AppData\Local\rbridge`,
		CacheDir:     `C:\Users\u\AppData\Local\rbridge\cache`,
		RuntimeDir:   `C:\Users\u\AppData\Local\rbridge\run`,
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
