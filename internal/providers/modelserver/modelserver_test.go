package modelserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

type recorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *recorder) Emit(e domain.Event) { r.mu.Lock(); r.events = append(r.events, e); r.mu.Unlock() }

func (r *recorder) count(t domain.EventType) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == t {
			n++
		}
	}
	return n
}

// fakeServer answers like Ollama or an OpenAI-compatible server. Each reply
// says how many messages it received, so tests can check the history.
func fakeServer(t *testing.T, api API) (*httptest.Server, *[]map[string]any) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"version":"0.12.3"}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"qwen3:0.6b"}]}`)
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"local-model"}]}`)
		case "/api/chat", "/v1/chat/completions":
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			bodies = append(bodies, body)
			mu.Unlock()
			if body["model"] == "missing" {
				http.Error(w, `{"error":"model not found"}`, http.StatusNotFound)
				return
			}
			msgs := body["messages"].([]any)
			last := msgs[len(msgs)-1].(map[string]any)["content"].(string)
			words := []string{"heard ", fmt.Sprint(len(msgs)), " messages"}
			if last == "slow" {
				words = nil
				for i := 0; i < 200; i++ {
					words = append(words, ".")
				}
			}
			if last == "json" {
				words = []string{`{"ok":`, `true}`}
			}
			fl := w.(http.Flusher)
			for _, wd := range words {
				if api == APIOpenAI {
					b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": wd}}}})
					fmt.Fprintf(w, "data: %s\n\n", b)
				} else {
					b, _ := json.Marshal(map[string]any{"message": map[string]any{"role": "assistant", "content": wd}, "done": false})
					fmt.Fprintf(w, "%s\n", b)
				}
				fl.Flush()
				if last == "slow" {
					select {
					case <-r.Context().Done():
						return
					case <-time.After(20 * time.Millisecond):
					}
				}
			}
			if api == APIOpenAI {
				fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
			} else {
				fmt.Fprint(w, `{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":7,"eval_count":3}`+"\n")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func open(t *testing.T, api API, opts domain.SessionOptions) (*Session, *recorder, *[]map[string]any) {
	t.Helper()
	srv, bodies := fakeServer(t, api)
	rec := &recorder{}
	if opts.Model == "" {
		opts.Model = "m"
	}
	s, err := Open(context.Background(), Server{URL: srv.URL, API: api}, nil, nil, opts, rec)
	if err != nil {
		t.Fatal(err)
	}
	return s, rec, bodies
}

func TestChat(t *testing.T) {
	for _, api := range []API{APIOllama, APIOpenAI} {
		t.Run(string(api), func(t *testing.T) {
			s, rec, _ := open(t, api, domain.SessionOptions{})
			ctx := context.Background()
			res, err := s.Prompt(ctx, domain.Turn{Prompt: domain.Text("hello")})
			if err != nil || res.Output != "heard 1 messages" || res.StopReason != domain.StopEndTurn {
				t.Fatalf("%+v %v", res, err)
			}
			if res.Usage == nil || res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 3 {
				t.Fatalf("usage %+v", res.Usage)
			}
			// The history is sent again with the next turn.
			if res, _ := s.Prompt(ctx, domain.Turn{Prompt: domain.Text("again")}); res.Output != "heard 3 messages" {
				t.Fatalf("second turn %q", res.Output)
			}
			if rec.count(domain.EventMessageDelta) < 3 || rec.count(domain.EventSessionStarted) != 1 {
				t.Fatalf("events %+v", rec.events)
			}
		})
	}
}

func TestStructuredAndCancel(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	s, _, bodies := open(t, APIOllama, domain.SessionOptions{OutputSchema: schema})
	res, err := s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("json")})
	if err != nil || string(res.Structured) != `{"ok":true}` {
		t.Fatalf("%+v %v", res, err)
	}
	if (*bodies)[0]["format"] == nil {
		t.Fatal("schema not passed to the server")
	}
	go func() { time.Sleep(100 * time.Millisecond); s.Cancel(context.Background()) }()
	start := time.Now()
	res, err = s.Prompt(context.Background(), domain.Turn{Prompt: domain.Text("slow")})
	if err != nil || res.StopReason != domain.StopCancelled || time.Since(start) > 3*time.Second {
		t.Fatalf("cancel: %+v %v after %s", res, err, time.Since(start))
	}
}

func TestRefusals(t *testing.T) {
	srv, _ := fakeServer(t, APIOllama)
	server := Server{URL: srv.URL, API: APIOllama}
	ctx := context.Background()
	for name, opts := range map[string]domain.SessionOptions{
		"skills": {Model: "m", SkillDirs: []string{"/x"}},
		"mcp":    {Model: "m", MCPServers: []domain.MCPServer{{Name: "x"}}},
		"resume": {Model: "m", ResumeNativeID: "old"},
	} {
		if _, err := Open(ctx, server, nil, nil, opts, nil); domain.CodeOf(err) != domain.ErrCapabilityUnavailable {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := Open(ctx, server, nil, nil, domain.SessionOptions{}, nil); domain.CodeOf(err) != domain.ErrInvalidInput || !strings.Contains(err.Error(), "qwen3:0.6b") {
		t.Errorf("no model: %v", err)
	}
	if _, err := Open(ctx, Server{URL: "http://127.0.0.1:1", API: APIOllama}, nil, nil, domain.SessionOptions{Model: "m"}, nil); domain.CodeOf(err) != domain.ErrProviderUnavailable {
		t.Errorf("no server: %v", err)
	}
	s, err := Open(ctx, server, nil, nil, domain.SessionOptions{Model: "missing"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, domain.Turn{Prompt: domain.Text("x")}); domain.CodeOf(err) != domain.ErrInvalidInput {
		t.Errorf("unknown model: %v", err)
	}
	img := []domain.ContentBlock{{Type: "image", Data: "x"}}
	if _, err := s.Prompt(ctx, domain.Turn{Prompt: img}); domain.CodeOf(err) != domain.ErrCapabilityUnavailable {
		t.Errorf("image: %v", err)
	}
}

func TestNormalizeURL(t *testing.T) {
	good := map[string]string{
		"127.0.0.1:11434":         "http://127.0.0.1:11434",
		"0.0.0.0:11434":           "http://127.0.0.1:11434",
		"http://localhost:8080/":  "http://localhost:8080",
		"https://[::1]:9000/base": "https://[::1]:9000/base",
	}
	for in, want := range good {
		if got, err := NormalizeURL(in); err != nil || got != want {
			t.Errorf("%s -> %s %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "http://192.168.1.5:11434", "https://api.example.com", "ftp://127.0.0.1"} {
		if _, err := NormalizeURL(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
