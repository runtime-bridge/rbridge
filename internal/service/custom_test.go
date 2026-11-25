//go:build unix

package service

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/providers/acp"
	"github.com/runtime-bridge/rbridge/internal/providers/contract"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
)

// buildFakeAgent compiles the standalone fake agent once per test binary.
func buildFakeAgent(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	out := filepath.Join(t.TempDir(), "my-agent")
	cmd := exec.Command("go", "build", "-o", out, "github.com/runtime-bridge/rbridge/internal/tools/fakeagent")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake agent: %v %s", err, b)
	}
	return out
}

// TestCustomProviderFromConfig adds an ACP agent through configuration only
// and runs the provider contract suite against it.
func TestCustomProviderFromConfig(t *testing.T) {
	bin := buildFakeAgent(t)
	h := newRunHarness(t)
	h.cfg.Providers.Custom = []config.CustomProvider{{
		ID: "my-agent", Integration: "acp", VersionArgs: []string{"--version"},
		Launch: config.LaunchConfig{Command: bin, EnvPassthrough: []string{fakeacp.EnvScenario}},
	}}
	svc := h.svc()
	st, err := svc.Provider(context.Background(), "my-agent", true)
	if err != nil || st.Runtime.Health != domain.HealthAvailable || st.Runtime.Version != "1.19.6" {
		t.Fatalf("detection: %+v %v", st.Runtime, err)
	}
	m, _ := svc.registry.Get("my-agent")
	contract.Run(t, func(ctx context.Context, t *testing.T, scenario string, opts domain.SessionOptions, sink domain.EventSink) (domain.ProviderSession, error) {
		opts.Executable = st.Runtime.Executable
		opts.Env = []string{"PATH=" + os.Getenv("PATH"), fakeacp.EnvScenario + "=" + scenario}
		return acp.Open(ctx, m.Capabilities, acp.Launch{Args: svc.launchArgs(m)}, opts, sink)
	})

	// The same provider also runs whole tasks through the service.
	res, err := svc.Run(context.Background(), domain.Task{Workspace: h.workspace(), Prompt: domain.Text("custom agent"), Provider: "my-agent", Timeout: time.Minute}, RunOptions{})
	if err != nil || res.Output != "echo: custom agent" {
		t.Fatalf("run: %+v %v", res, err)
	}
}
