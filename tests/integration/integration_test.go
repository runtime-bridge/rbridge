//go:build integration

// Package integration runs rbridge against the real runtimes installed on the
// machine. It costs plan usage or local compute, so it only runs on request:
//
//	RBRIDGE_IT_PROVIDERS=claude-code,opencode \
//	RBRIDGE_IT_MODELS=claude-code=haiku,opencode=ollama/qwen3:8b \
//	RBRIDGE_IT_REPORT=/tmp/compat.md \
//	go test -tags integration -v -timeout 60m ./tests/integration/
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rbridge/rbridge/internal/config"
	"github.com/rbridge/rbridge/internal/daemon"
	"github.com/rbridge/rbridge/internal/service"
	"github.com/rbridge/rbridge/pkg/bridge"
)

func providers() []string {
	v := os.Getenv("RBRIDGE_IT_PROVIDERS")
	if v == "" {
		v = "claude-code"
	}
	return strings.Split(v, ",")
}

func model(provider string) string {
	for _, kv := range strings.Split(os.Getenv("RBRIDGE_IT_MODELS"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == provider {
			return v
		}
	}
	if provider == "claude-code" {
		return "haiku"
	}
	return ""
}

func repoRoot(t *testing.T) string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

func newBridge(t *testing.T) *bridge.Bridge {
	t.Helper()
	b, err := bridge.New(context.Background(), bridge.Options{Home: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func requireAvailable(t *testing.T, b *bridge.Bridge, id string) {
	t.Helper()
	list, err := b.Providers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID == id && p.Runtime.Health == "available" {
			return
		}
	}
	t.Skipf("%s is not available on this machine", id)
}

func sampleProject(t *testing.T) string {
	t.Helper()
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	os.WriteFile(filepath.Join(ws, "app.py"), []byte(`import os, sqlite3, hashlib

API_KEY = "sk_live_51HxExampleNotReal"
password = "hunter2"

def run(cmd):
    os.system("ls " + cmd)

def find_user(name):
    db = sqlite3.connect("users.db")
    return db.execute("SELECT * FROM users WHERE name = '%s'" % name).fetchall()

def store(pw):
    return hashlib.md5(pw.encode()).hexdigest()
`), 0o644)
	os.WriteFile(filepath.Join(ws, "util.py"), []byte("def add(a, b):\n    return a + b\n"), 0o644)
	return ws
}

func opts(provider string) []bridge.RunOption {
	o := []bridge.RunOption{bridge.AllowAll()}
	if m := model(provider); m != "" {
		o = append(o, bridge.WithModel(m))
	}
	return o
}

func TestBasicTurn(t *testing.T) {
	for _, p := range providers() {
		t.Run(p, func(t *testing.T) {
			b := newBridge(t)
			requireAvailable(t, b, p)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			res, err := b.Run(ctx, bridge.Task{Workspace: sampleProject(t), Prompt: bridge.Text("Reply with exactly the word: pong"), Provider: p}, opts(p)...)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ToLower(res.Output), "pong") {
				t.Fatalf("output %q", res.Output)
			}
		})
	}
}

func TestCancelRealTurn(t *testing.T) {
	for _, p := range providers() {
		t.Run(p, func(t *testing.T) {
			b := newBridge(t)
			requireAvailable(t, b, p)
			s, err := b.Open(context.Background(), bridge.Task{Workspace: sampleProject(t), Provider: p}, opts(p)...)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(5*time.Second, cancel)
			start := time.Now()
			res, err := s.Prompt(ctx, bridge.Text("Write a 3000 word essay about rivers. Do not use any tools."))
			if err != nil {
				t.Fatal(err)
			}
			if res.StopReason != bridge.StopCancelled {
				t.Fatalf("stop %s", res.StopReason)
			}
			if time.Since(start) > 60*time.Second {
				t.Fatal("cancel took too long")
			}
		})
	}
}

// compat is one line of the compatibility table.
type compat struct {
	Provider, Skill                    string
	Discovered, Instructions, Ref, Run string
	Output                             string
	Seconds                            float64
	Err                                string
}

var corpus = []struct {
	name, skillMarker, refMarker, scriptMarker, prompt string
	json                                               bool
}{
	{"doc-writer", "RB-DOC-1001", "", "", "Use the doc-writer skill to document util.py.", false},
	{"code-review", "RB-REVIEW-2001", "RB-REVIEW-2002", "", "Use the code-review skill to review app.py.", false},
	{"security-audit", "RB-SKILL-7731", "RB-REF-4402", "RB-SCRIPT-9150", "Use the security-audit skill to audit this project.", false},
	{"report-builder", "RB-REPORT-4001", "", "", "Use the report-builder skill to report on this project.", true},
	{"project-stats", "", "", "RB-STATS-5001", "Use the project-stats skill on this project.", false},
}

func TestSkillCompatibility(t *testing.T) {
	var mu sync.Mutex
	var rows []compat
	defer func() { writeReport(t, rows) }()
	for _, p := range providers() {
		for _, c := range corpus {
			t.Run(p+"/"+c.name, func(t *testing.T) {
				b := newBridge(t)
				requireAvailable(t, b, p)
				ctx := context.Background()
				if _, err := b.AddSkill(ctx, filepath.Join(repoRoot(t), "testdata", "skills", c.name), false); err != nil {
					t.Fatal(err)
				}
				if _, err := b.SetSkillTrust(ctx, c.name, bridge.TrustTrusted); err != nil {
					t.Fatal(err)
				}
				var events strings.Builder
				discovered := "n/a"
				var emu sync.Mutex
				onEvent := bridge.OnEvent(func(e bridge.Event) {
					emu.Lock()
					defer emu.Unlock()
					events.Write(e.Data)
					if e.Type == bridge.EventSessionStarted {
						var d bridge.SessionStartedData
						json.Unmarshal(e.Data, &d)
						if d.Skills != nil {
							discovered = "no"
							for _, s := range d.Skills {
								if s == c.name {
									discovered = "yes"
								}
							}
						}
					}
				})
				rctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
				defer cancel()
				start := time.Now()
				res, err := b.Run(rctx, bridge.Task{Workspace: sampleProject(t), Prompt: bridge.Text(c.prompt), Skills: []string{c.name}, Provider: p}, append(opts(p), onEvent)...)
				row := compat{Provider: p, Skill: c.name, Discovered: discovered, Seconds: time.Since(start).Seconds()}
				all := res.Output + events.String()
				mark := func(m string) string {
					if m == "" {
						return "n/a"
					}
					if strings.Contains(all, m) {
						return "yes"
					}
					return "no"
				}
				row.Instructions, row.Ref, row.Run = mark(c.skillMarker), mark(c.refMarker), mark(c.scriptMarker)
				row.Output = "ok"
				if c.json {
					out := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(res.Output), "```json"), "```"))
					if !json.Valid([]byte(out)) {
						row.Output = "not JSON"
					}
				}
				if err != nil {
					row.Err = err.Error()
					row.Output = "error"
				}
				mu.Lock()
				rows = append(rows, row)
				mu.Unlock()
				t.Logf("%+v", row)
				if err != nil {
					t.Error(err)
				}
			})
		}
	}
}

func writeReport(t *testing.T, rows []compat) {
	path := os.Getenv("RBRIDGE_IT_REPORT")
	if path == "" || len(rows) == 0 {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# rbridge skill compatibility\n\nGenerated %s by tests/integration.\n\n", time.Now().UTC().Format(time.RFC3339))
	b.WriteString("| Provider | Skill | Discovered | Instructions | Reference | Script | Output | Seconds | Error |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %.0f | %s |\n", r.Provider, r.Skill, r.Discovered, r.Instructions, r.Ref, r.Run, r.Output, r.Seconds, strings.ReplaceAll(r.Err, "|", "/"))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Error(err)
	}
}

func TestDaemonRealRun(t *testing.T) {
	home := t.TempDir()
	b := newBridgeAt(t, home)
	requireAvailable(t, b, providers()[0])
	p := providers()[0]
	vars := func(k string) string {
		if k == "RBRIDGE_HOME" {
			return home
		}
		return os.Getenv(k)
	}
	h, _ := os.UserHomeDir()
	svc, err := service.FromEnvironment(service.Environment{Env: config.Env{GOOS: runtime.GOOS, Getenv: vars, HomeDir: h, UID: os.Getuid()}, Environ: os.Environ})
	if err != nil {
		t.Fatal(err)
	}
	files := daemon.FilesFor(svc.Paths())
	short, _ := os.MkdirTemp("", "rbi")
	defer os.RemoveAll(short)
	files.Dir, files.Socket, files.State, files.Token = short, filepath.Join(short, "s.sock"), filepath.Join(short, "d.json"), filepath.Join(short, "t")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- daemon.Run(ctx, daemon.Options{Service: svc, Config: svc.Config(), Files: files, Ready: func(daemon.State) { close(ready) }})
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	c, err := bridge.Connect(context.Background(), bridge.ConnectOptions{Socket: files.Socket, Client: "it"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer rcancel()
	res, err := c.Run(rctx, bridge.Task{Workspace: sampleProject(t), Prompt: bridge.Text("Reply with exactly the word: pong"), Provider: p}, opts(p)...)
	if err != nil || !strings.Contains(strings.ToLower(res.Output), "pong") {
		t.Fatalf("%+v %v", res, err)
	}
}

func newBridgeAt(t *testing.T, home string) *bridge.Bridge {
	b, err := bridge.New(context.Background(), bridge.Options{Home: home})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
