//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/service"
)

type run struct {
	code   int
	stdout string
	stderr string
}

func setup(t *testing.T, exes map[string]string) func(args ...string) run {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, script := range exes {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	vars := map[string]string{"PATH": bin, "RBRIDGE_HOME": filepath.Join(home, "rb")}
	return func(args ...string) run {
		var out, errb bytes.Buffer
		code := Run(context.Background(), args, Env{
			Stdout: &out, Stderr: &errb,
			Getenv:  func(k string) string { return vars[k] },
			Environ: func() []string { return []string{"PATH=/usr/bin:/bin"} },
			Home:    home, Dir: home, GOOS: "linux", UID: 1000,
		})
		return run{code, out.String(), errb.String()}
	}
}

var bothInstalled = map[string]string{
	"claude":   `echo "2.1.331 (Claude Code)"`,
	"opencode": `echo 1.19.6`,
}

func TestProvidersTable(t *testing.T) {
	r := setup(t, bothInstalled)("providers")
	if r.code != 0 {
		t.Fatalf("code %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"PROVIDER", "claude-code", "2.1.331", "claude-headless", "opencode", "1.19.6", "acp"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
}

func TestProvidersJSON(t *testing.T) {
	r := setup(t, bothInstalled)("providers", "--json", "--refresh")
	var list []service.ProviderStatus
	if err := json.Unmarshal([]byte(r.stdout), &list); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, r.stdout)
	}
	if len(list) != 6 || list[1].Runtime.Version != "1.19.6" {
		t.Fatalf("list = %+v", list)
	}
}

func TestProvidersShow(t *testing.T) {
	cli := setup(t, bothInstalled)
	r := cli("providers", "show", "opencode")
	if r.code != 0 {
		t.Fatalf("code %d: %s", r.code, r.stderr)
	}
	for _, want := range []string{"OpenCode (opencode)", "sessions.load", "Skill locations:", ".agents/skills", "One prompt at a time"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("missing %q in:\n%s", want, r.stdout)
		}
	}
	// Flags after the positional argument work too.
	if r := cli("providers", "show", "opencode", "--json"); r.code != 0 || !strings.HasPrefix(r.stdout, "{") {
		t.Errorf("show --json: %d %s", r.code, r.stdout)
	}
}

func TestErrorsAndExitCodes(t *testing.T) {
	cli := setup(t, nil)
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"providers", "show", "nope"}, 10, "provider_not_found"},
		{[]string{"providers", "show"}, 2, "usage"},
		{[]string{"providers", "extra"}, 2, "unexpected argument"},
		{[]string{"bogus"}, 2, "unknown command"},
		{[]string{"providers", "--nope"}, 2, "flag provided but not defined"},
		{nil, 2, "Usage"},
	}
	for _, c := range cases {
		r := cli(c.args...)
		if r.code != c.code || !strings.Contains(r.stderr, c.msg) {
			t.Errorf("%v: code %d stderr %q", c.args, r.code, r.stderr)
		}
	}
}

func TestDoctorExitCode(t *testing.T) {
	if r := setup(t, nil)("doctor"); r.code != 1 || !strings.Contains(r.stdout, "[FAIL] runtimes") {
		t.Fatalf("no runtimes: code %d\n%s", r.code, r.stdout)
	}
	r := setup(t, bothInstalled)("doctor")
	if r.code != 0 || !strings.Contains(r.stdout, "[OK] provider opencode") {
		t.Fatalf("code %d\n%s", r.code, r.stdout)
	}
}

func TestInvalidConfig(t *testing.T) {
	home := t.TempDir()
	rb := filepath.Join(home, "rb")
	if err := os.MkdirAll(filepath.Join(rb, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(rb, "config", "config.yaml"), []byte("policy:\n  default_profile: yolo\n"), 0o644)
	var out, errb bytes.Buffer
	code := Run(context.Background(), []string{"providers"}, Env{
		Stdout: &out, Stderr: &errb,
		Getenv:  func(k string) string { return map[string]string{"RBRIDGE_HOME": rb}[k] },
		Environ: func() []string { return nil },
		Home:    home, Dir: home, GOOS: "linux",
	})
	if code != 2 || !strings.Contains(errb.String(), "default_profile") {
		t.Fatalf("code %d stderr %q", code, errb.String())
	}
}

func TestVersionAndHelp(t *testing.T) {
	cli := setup(t, nil)
	if r := cli("version"); r.code != 0 || !strings.HasPrefix(r.stdout, "rbridge ") {
		t.Errorf("version: %+v", r)
	}
	if r := cli("version", "--json"); r.code != 0 || !strings.Contains(r.stdout, `"go_version"`) {
		t.Errorf("version --json: %+v", r)
	}
	if r := cli("help"); r.code != 0 || !strings.Contains(r.stdout, "rbridge doctor") {
		t.Errorf("help: %+v", r)
	}
}
