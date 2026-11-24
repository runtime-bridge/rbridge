package registry

import (
	"slices"
	"testing"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/domain"
)

func ids(r *Registry) []string {
	var out []string
	for _, m := range r.All() {
		out = append(out, m.ID)
	}
	return out
}

func TestOrderAndCustom(t *testing.T) {
	cfg := config.Defaults()
	cfg.Providers.Order = []string{"opencode", "my-agent"}
	cfg.Providers.Custom = []config.CustomProvider{{ID: "my-agent", Launch: config.LaunchConfig{Command: "/opt/my-agent", Args: []string{"--acp"}}}}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(r); !slices.Equal(got, []string{"opencode", "my-agent", "claude-acp", "claude-code", "codex", "gemini-cli"}) {
		t.Fatalf("order = %v", got)
	}
	m, ok := r.Get("my-agent")
	if !ok || m.Class != domain.ClassCustom || m.Integration != domain.IntegrationACP || m.Launch.Args[0] != "--acp" {
		t.Fatalf("custom manifest = %+v", m)
	}
	if len(m.Capabilities) != 0 {
		t.Error("custom capabilities must start unknown")
	}
}

func TestRejects(t *testing.T) {
	dup := config.Defaults()
	dup.Providers.Custom = []config.CustomProvider{{ID: "opencode", Launch: config.LaunchConfig{Command: "/x"}}}
	if _, err := New(dup); err == nil {
		t.Error("custom provider may not reuse a built-in id")
	}
	unknownOrder := config.Defaults()
	unknownOrder.Providers.Order = []string{"nope"}
	if _, err := New(unknownOrder); err == nil {
		t.Error("unknown provider in order accepted")
	}
	unknownSetting := config.Defaults()
	unknownSetting.Providers.Settings["nope"] = config.ProviderEntry{}
	if _, err := New(unknownSetting); err == nil {
		t.Error("settings for unknown provider accepted")
	}
}

func TestBuiltInCapabilitiesHaveManifestSource(t *testing.T) {
	for _, m := range BuiltIns() {
		for _, n := range domain.KnownCapabilities {
			c, ok := m.Capabilities[n]
			if !ok {
				t.Errorf("%s: capability %s not declared", m.ID, n)
				continue
			}
			if c.Source != domain.SourceManifest {
				t.Errorf("%s: %s source = %s", m.ID, n, c.Source)
			}
		}
		if !m.BuiltIn || m.Class != domain.ClassAgentRuntime {
			t.Errorf("%s: unexpected class or flag", m.ID)
		}
	}
}
