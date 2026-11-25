//go:build unix

package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
)

func TestModelServerProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.12.3"}`)
		case "/api/chat":
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"local "},"done":false}`)
			fmt.Fprintln(w, `{"message":{"role":"assistant","content":"answer"},"done":true,"prompt_eval_count":5,"eval_count":2}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	h := newRunHarness(t)
	h.cfg.Providers.Custom = []config.CustomProvider{{ID: "local-llm", Integration: "model-server", URL: srv.URL, API: "ollama"}}
	h.cfg.Providers.Settings = map[string]config.ProviderEntry{"local-llm": {Model: "qwen3:0.6b"}}
	svc := h.svc()
	ctx := context.Background()

	st, err := svc.Provider(ctx, "local-llm", true)
	if err != nil || st.Runtime.Health != domain.HealthAvailable || st.Runtime.Version != "0.12.3" || st.Runtime.AuthMode != domain.AuthLocal {
		t.Fatalf("detection %+v %v", st.Runtime, err)
	}
	var notices []string
	res, err := svc.Run(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("hi"), Provider: "local-llm"},
		RunOptions{Sink: domain.EventSinkFunc(func(e domain.Event) {
			if e.Type == domain.EventNotice {
				notices = append(notices, string(e.Data))
			}
		})})
	if err != nil || res.Output != "local answer" || res.Usage == nil || res.Usage.OutputTokens != 2 {
		t.Fatalf("run %+v %v", res, err)
	}
	if !strings.Contains(strings.Join(notices, " "), "enforced by rbridge: tools, network; relies on the runtime asking: none") {
		t.Fatalf("enforcement notice %v", notices)
	}

	// Skills and MCP servers are refused, not ignored.
	withSkill := domain.Task{Workspace: h.workspace(), Prompt: domain.Text("hi"), Provider: "local-llm", Skills: []string{"x"}}
	if _, err := svc.Run(ctx, withSkill, RunOptions{}); domain.CodeOf(err) != domain.ErrCapabilityUnavailable {
		t.Fatalf("skills: %v", err)
	}

	// It is never chosen without being named.
	off := false
	h.cfg.Providers.Settings["claude-code"] = config.ProviderEntry{Enabled: &off}
	h.cfg.Providers.Settings["opencode"] = config.ProviderEntry{Enabled: &off}
	_, err = h.svc().Run(ctx, domain.Task{Workspace: h.workspace(), Prompt: domain.Text("hi")}, RunOptions{})
	if domain.CodeOf(err) != domain.ErrCapabilityUnavailable || !strings.Contains(err.Error(), "local-llm: a model server is used only when a task names it") {
		t.Fatalf("auto choice: %v", err)
	}
}
