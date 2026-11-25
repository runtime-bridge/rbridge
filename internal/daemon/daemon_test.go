//go:build unix

package daemon_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/daemon"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
	"github.com/runtime-bridge/rbridge/internal/providers/detect"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeacp"
	"github.com/runtime-bridge/rbridge/internal/providers/fakeclaude"
	"github.com/runtime-bridge/rbridge/internal/service"
	"github.com/runtime-bridge/rbridge/internal/store"
	"github.com/runtime-bridge/rbridge/pkg/bridge"
)

func TestMain(m *testing.M) {
	if os.Getenv("RBRIDGE_FAKE_SVC") == "1" {
		switch filepath.Base(os.Args[0]) {
		case "claude":
			fakeclaude.Main()
		case "opencode":
			fakeacp.Main()
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type env struct {
	t     *testing.T
	ws    string
	files daemon.Files
	state daemon.State
	token string
	stop  func()
	done  chan error
}

// start runs a daemon over fake runtimes. scenarios sets the fake behaviour.
func start(t *testing.T, scenarios map[string]string) *env {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	bin := filepath.Join(home, "bin")
	os.MkdirAll(bin, 0o755)
	exe, _ := os.Executable()
	for _, n := range []string{"claude", "opencode"} {
		os.Symlink(exe, filepath.Join(bin, n))
	}
	vars := map[string]string{"PATH": bin + ":/usr/bin:/bin", "HOME": home, "RBRIDGE_FAKE_SVC": "1"}
	for k, v := range scenarios {
		vars[k] = v
	}
	getenv := func(k string) string { return vars[k] }
	environ := func() []string {
		var out []string
		for k, v := range vars {
			out = append(out, k+"="+v)
		}
		return out
	}
	cfg := config.Defaults()
	cfg.Policy.EnvAllowlist = append(cfg.Policy.EnvAllowlist, "RBRIDGE_FAKE_SVC", fakeacp.EnvScenario, fakeclaude.EnvScenario)
	cfg.Execution.KillGracePeriod = time.Second
	cfg.Daemon.HTTP.Enabled = true
	cfg.Daemon.HTTP.Port = 0
	cfg.Daemon.RequestsPerSecond = 1000
	cfg.Execution.MaxConcurrentSessions = 20
	paths, _ := config.ResolvePaths(config.Env{GOOS: "linux", Getenv: func(k string) string {
		if k == "RBRIDGE_HOME" {
			return filepath.Join(home, "rb")
		}
		return ""
	}})
	// Unix socket paths are limited to about 100 bytes; keep the runtime dir short.
	short, err := os.MkdirTemp("", "rbd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(short) })
	paths.RuntimeDir = short
	det := &detect.Detector{FS: detect.OSFS, Run: process.Run, Getenv: getenv, Environ: environ, GOOS: "linux", Home: home, EnvAllowlist: cfg.Policy.EnvAllowlist, Timeout: 5 * time.Second}
	svc, err := service.New(service.Options{Paths: paths, Config: cfg, Store: store.NewMemStore(), Detector: det, Getenv: getenv, Environ: environ})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, ws: filepath.Join(home, "ws"), files: daemon.FilesFor(paths), done: make(chan error, 1)}
	os.MkdirAll(e.ws, 0o755)
	ready := make(chan daemon.State, 1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		e.done <- daemon.Run(ctx, daemon.Options{Service: svc, Config: cfg, Files: e.files, Ready: func(s daemon.State) { ready <- s }})
	}()
	select {
	case e.state = <-ready:
	case err := <-e.done:
		t.Fatalf("daemon did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("daemon start timeout")
	}
	b, _ := os.ReadFile(e.files.Token)
	e.token = string(b)
	var once sync.Once
	e.stop = func() {
		once.Do(func() {
			cancel()
			<-e.done
		})
	}
	t.Cleanup(e.stop)
	return e
}

func (e *env) req(method, path, client string, body any, hdr map[string]string) (*http.Response, []byte) {
	e.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	r, _ := http.NewRequest(method, e.state.HTTP+path, rd)
	r.Header.Set("Authorization", "Bearer "+e.token)
	if client != "" {
		r.Header.Set("X-Rbridge-Client", client)
	}
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

func TestHTTPGuards(t *testing.T) {
	e := start(t, nil)
	check := func(name string, want int, method, path string, body any, hdr map[string]string) {
		t.Helper()
		resp, b := e.req(method, path, "", body, hdr)
		if resp.StatusCode != want {
			t.Errorf("%s: status %d want %d (%s)", name, resp.StatusCode, want, b)
		}
	}
	check("health without token", 200, "GET", "/v1/health", nil, map[string]string{"Authorization": ""})
	check("no token", 401, "GET", "/v1/providers", nil, map[string]string{"Authorization": ""})
	check("wrong token", 401, "GET", "/v1/providers", nil, map[string]string{"Authorization": "Bearer nope"})
	check("foreign origin", 403, "GET", "/v1/providers", nil, map[string]string{"Origin": "https://evil.example"})
	check("rebinding host", 421, "GET", "/v1/providers", nil, map[string]string{"Host": "evil.example:7432"})
	check("bad client", 400, "GET", "/v1/providers", nil, map[string]string{"X-Rbridge-Client": "../x"})
	check("too large", 413, "POST", "/v1/sessions", map[string]string{"pad": strings.Repeat("x", 2<<20)}, nil)
	check("ok", 200, "GET", "/v1/providers", nil, nil)
	check("missing session", 404, "GET", "/v1/sessions/ses_none", nil, nil)
}

// answer approves from another goroutine, so it must not call t.Fatal.
func (e *env) answer(session, approval string) {
	r, _ := http.NewRequest("POST", e.state.HTTP+"/v1/sessions/"+session+"/approvals/"+approval, strings.NewReader(`{"allow":true}`))
	r.Header.Set("Authorization", "Bearer "+e.token)
	r.Header.Set("X-Rbridge-Client", "app")
	if resp, err := http.DefaultClient.Do(r); err == nil {
		resp.Body.Close()
	}
}

// sse reads events from a stream until it ends or the context expires.
func (e *env) sse(ctx context.Context, client, id string, after int64, onEvent func(domain.Event) bool) []domain.Event {
	r, _ := http.NewRequestWithContext(ctx, "GET", e.state.HTTP+"/v1/sessions/"+id+"/events", nil)
	r.Header.Set("Authorization", "Bearer "+e.token)
	r.Header.Set("X-Rbridge-Client", client)
	if after > 0 {
		r.Header.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var out []domain.Event
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "event: end" {
			return out
		}
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			var ev domain.Event
			if json.Unmarshal([]byte(data), &ev) == nil && ev.Seq > 0 {
				out = append(out, ev)
				if onEvent != nil && !onEvent(ev) {
					return out
				}
			}
		}
	}
	return out
}

func TestHTTPSessionFlowAndReplay(t *testing.T) {
	e := start(t, nil)
	resp, b := e.req("POST", "/v1/sessions", "app-a", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "opencode"}}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("open: %d %s", resp.StatusCode, b)
	}
	var rec domain.SessionRecord
	json.Unmarshal(b, &rec)
	resp, b = e.req("POST", "/v1/sessions/"+rec.ID+"/turns", "app-a", map[string]any{"prompt": "over http"}, nil)
	var res domain.Result
	json.Unmarshal(b, &res)
	if resp.StatusCode != 200 || !strings.Contains(res.Output, "over http") {
		t.Fatalf("turn: %d %s", resp.StatusCode, b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	all := e.sse(ctx, "app-a", rec.ID, 0, nil)
	cancel()
	if len(all) < 4 {
		t.Fatalf("replay too short: %d", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i].Seq <= all[i-1].Seq {
			t.Fatal("events out of order")
		}
	}
	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	tail := e.sse(ctx, "app-a", rec.ID, all[len(all)-2].Seq, nil)
	cancel()
	if len(tail) != 1 || tail[0].Seq != all[len(all)-1].Seq {
		t.Fatalf("Last-Event-ID replay: %+v", tail)
	}
	// Other clients cannot see or touch the session.
	if resp, _ := e.req("GET", "/v1/sessions/"+rec.ID, "app-b", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("app-b can see app-a's session: %d", resp.StatusCode)
	}
	if resp, _ := e.req("POST", "/v1/sessions/"+rec.ID+"/turns", "app-b", map[string]any{"prompt": "x"}, nil); resp.StatusCode != 404 {
		t.Fatalf("app-b can prompt app-a's session: %d", resp.StatusCode)
	}
	if _, b := e.req("GET", "/v1/sessions", "app-b", nil, nil); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("app-b list: %s", b)
	}
	// Closing ends the stream.
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ended := make(chan []domain.Event, 1)
	go func() { ended <- e.sse(ctx, "app-a", rec.ID, all[len(all)-1].Seq, nil) }()
	time.Sleep(200 * time.Millisecond)
	if resp, b := e.req("DELETE", "/v1/sessions/"+rec.ID, "app-a", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("close: %s", b)
	}
	select {
	case evs := <-ended:
		if len(evs) == 0 || evs[len(evs)-1].Type != domain.EventSessionClosed {
			t.Fatalf("stream did not end with session.closed: %+v", evs)
		}
	case <-ctx.Done():
		t.Fatal("stream did not end")
	}
	if _, b := e.req("GET", "/v1/sessions/"+rec.ID, "app-a", nil, nil); !strings.Contains(string(b), `"closed"`) {
		t.Fatalf("stored record: %s", b)
	}
}

func TestApprovalsOverHTTP(t *testing.T) {
	e := start(t, map[string]string{fakeclaude.EnvScenario: "permission"})
	_, b := e.req("POST", "/v1/sessions", "app", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "claude-code", "profile": "controlled"}}, nil)
	var rec domain.SessionRecord
	json.Unmarshal(b, &rec)
	if resp, b := e.req("POST", "/v1/sessions/"+rec.ID+"/turns", "app", map[string]any{"prompt": "write", "wait": false}, nil); resp.StatusCode != 200 {
		t.Fatalf("start turn: %s", b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var output string
	e.sse(ctx, "app", rec.ID, 0, func(ev domain.Event) bool {
		switch ev.Type {
		case domain.EventApprovalRequired:
			var req domain.ApprovalRequest
			json.Unmarshal(ev.Data, &req)
			go e.answer(rec.ID, req.ID)
		case domain.EventTurnCompleted:
			var d domain.TurnCompletedData
			json.Unmarshal(ev.Data, &d)
			output = d.Output
			return false
		}
		return true
	})
	if output != "allowed" {
		t.Fatalf("output %q", output)
	}
	if _, err := os.Stat(filepath.Join(e.ws, "notes.txt")); err != nil {
		t.Fatal("approved write did not happen")
	}
}

func TestConnectMatchesEmbedded(t *testing.T) {
	e := start(t, map[string]string{fakeclaude.EnvScenario: "permission"})
	ctx := context.Background()
	var c bridge.Client
	r, err := bridge.Connect(ctx, bridge.ConnectOptions{Socket: e.files.Socket, Client: "go-app"})
	if err != nil {
		t.Fatal(err)
	}
	c = r
	defer c.Close()
	ps, err := c.Providers(ctx, false)
	if err != nil || len(ps) != 6 {
		t.Fatalf("providers %+v %v", ps, err)
	}
	var mu sync.Mutex
	var types []domain.EventType
	res, err := c.Run(ctx, bridge.Task{Workspace: e.ws, Prompt: bridge.Text("hello daemon"), Provider: "opencode"},
		bridge.OnEvent(func(ev bridge.Event) { mu.Lock(); types = append(types, ev.Type); mu.Unlock() }))
	if err != nil || !strings.Contains(res.Output, "hello daemon") {
		t.Fatalf("run %+v %v", res, err)
	}
	mu.Lock()
	gotStarted := false
	for _, ty := range types {
		if ty == domain.EventSessionStarted {
			gotStarted = true
		}
	}
	mu.Unlock()
	if !gotStarted {
		t.Fatalf("events not forwarded: %v", types)
	}
	// Approvals reach the Go approver through the daemon.
	asked := false
	res, err = c.Run(ctx, bridge.Task{Workspace: e.ws, Prompt: bridge.Text("write"), Provider: "claude-code", Profile: "controlled"},
		bridge.WithApprover(func(_ context.Context, req bridge.ApprovalRequest) bridge.ApprovalDecision {
			asked = true
			return req.AllowOnce("user")
		}))
	if err != nil || res.Output != "allowed" || !asked {
		t.Fatalf("remote approval: %+v %v asked=%v", res, err, asked)
	}
	os.Remove(filepath.Join(e.ws, "notes.txt"))
	res, err = c.Run(ctx, bridge.Task{Workspace: e.ws, Prompt: bridge.Text("write"), Provider: "claude-code", Profile: "controlled"})
	if err != nil || res.Output != "denied" {
		t.Fatalf("no approver must deny: %+v %v", res, err)
	}
	list, err := c.Sessions(ctx)
	if err != nil || len(list) != 3 {
		t.Fatalf("sessions %d %v", len(list), err)
	}
	s, err := c.Resume(ctx, list[len(list)-1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, bridge.Text("again")); err != nil {
		t.Fatal(err)
	}
	s.Close(ctx)
	if _, err := c.Run(ctx, bridge.Task{Workspace: "relative", Prompt: bridge.Text("x")}); bridge.CodeOf(err) != bridge.ErrInvalidInput {
		t.Fatalf("errors keep their code over IPC: %v", err)
	}
}

func TestSingleInstanceAndCleanShutdown(t *testing.T) {
	e := start(t, nil)
	err := daemon.Run(context.Background(), daemon.Options{Files: e.files})
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Fatalf("second daemon: %v", err)
	}
	r, err := bridge.Connect(context.Background(), bridge.ConnectOptions{Socket: e.files.Socket})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(context.Background(), bridge.Task{Workspace: e.ws, Provider: "opencode"}); err != nil {
		t.Fatal(err)
	}
	e.stop()
	for _, p := range []string{e.files.Socket, e.files.State, e.files.Token} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind", p)
		}
	}
	if _, err := r.Providers(context.Background(), false); err == nil {
		t.Error("connection should fail after shutdown")
	}
}

func TestLongRuntimeDirGetsShortSocket(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("deep-directory-", 8))
	f := daemon.FilesFor(config.Paths{RuntimeDir: long})
	if len(f.Socket) > 100 || strings.HasPrefix(f.Socket, long) {
		t.Fatalf("socket %s (%d bytes)", f.Socket, len(f.Socket))
	}
	if f.Socket != daemon.FilesFor(config.Paths{RuntimeDir: long}).Socket {
		t.Fatal("socket path must be deterministic")
	}
	short := daemon.FilesFor(config.Paths{RuntimeDir: "/run/user/1000/rbridge"})
	if short.Socket != "/run/user/1000/rbridge/bridge.sock" {
		t.Fatalf("short dir: %s", short.Socket)
	}
}

func TestPerClientSessionLimit(t *testing.T) {
	e := start(t, nil)
	ctx := context.Background()
	var opened []string
	for i := 0; i < 8; i++ {
		resp, b := e.req("POST", "/v1/sessions", "greedy", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "opencode"}}, nil)
		if resp.StatusCode != 201 {
			t.Fatalf("session %d: %s", i, b)
		}
		var rec domain.SessionRecord
		json.Unmarshal(b, &rec)
		opened = append(opened, rec.ID)
	}
	if resp, b := e.req("POST", "/v1/sessions", "greedy", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "opencode"}}, nil); resp.StatusCode != 409 || !strings.Contains(string(b), "already has 8 open sessions") {
		t.Fatalf("limit not applied: %d %s", resp.StatusCode, b)
	}
	if resp, _ := e.req("POST", "/v1/sessions", "other", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "opencode"}}, nil); resp.StatusCode != 201 {
		t.Fatal("the limit is per client")
	}
	for _, id := range opened {
		e.req("DELETE", "/v1/sessions/"+id, "greedy", nil, nil)
	}
	_ = ctx
}

func TestModesOptionsAndInputOverDaemon(t *testing.T) {
	e := start(t, nil)
	ctx := context.Background()
	r, err := bridge.Connect(ctx, bridge.ConnectOptions{Socket: e.files.Socket, Client: "go-app"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := r.Open(ctx, bridge.Task{Workspace: e.ws, Provider: "opencode"}, bridge.WithMode("code"))
	if err != nil {
		t.Fatal(err)
	}
	cur, modes, err := s.Modes(ctx)
	if err != nil || cur != "code" || len(modes) != 2 {
		t.Fatalf("modes %s %v %v", cur, modes, err)
	}
	if err := s.SetMode(ctx, "ask"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOption(ctx, "model", "large"); err != nil {
		t.Fatal(err)
	}
	opts, err := s.Options(ctx)
	if err != nil || len(opts) != 1 || opts[0].Current != "large" {
		t.Fatalf("options %+v %v", opts, err)
	}
	if res, err := s.Prompt(ctx, bridge.Text("mode?")); err != nil || res.Output != "mode: ask model: large" {
		t.Fatalf("%+v %v", res, err)
	}
	s.Close(ctx)

	// Over HTTP: list modes and set an option.
	resp, body := e.req("POST", "/v1/sessions", "web", map[string]any{"task": map[string]any{"workspace": e.ws, "provider": "opencode"}}, nil)
	var rec domain.SessionRecord
	json.Unmarshal(body, &rec)
	if resp.StatusCode != 201 {
		t.Fatalf("open %d %s", resp.StatusCode, body)
	}
	resp, body = e.req("PUT", "/v1/sessions/"+rec.ID+"/options/model", "web", map[string]any{"value": "large"}, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"current":"large"`) {
		t.Fatalf("set option %d %s", resp.StatusCode, body)
	}
	resp, body = e.req("PUT", "/v1/sessions/"+rec.ID+"/mode", "web", map[string]any{"mode": "nope"}, nil)
	if resp.StatusCode != 400 {
		t.Fatalf("bad mode %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.req("GET", "/v1/sessions/"+rec.ID+"/modes", "other", nil, nil); resp.StatusCode != 404 {
		t.Fatal("another client must not see the session")
	}
	e.req("DELETE", "/v1/sessions/"+rec.ID, "web", nil, nil)
}

func TestInputOverDaemon(t *testing.T) {
	e := start(t, map[string]string{fakeacp.EnvScenario: "elicit"})
	ctx := context.Background()
	r, err := bridge.Connect(ctx, bridge.ConnectOptions{Socket: e.files.Socket})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	task := bridge.Task{Workspace: e.ws, Prompt: bridge.Text("x"), Provider: "opencode"}
	res, err := r.Run(ctx, task, bridge.WithInput(func(_ context.Context, req bridge.InputRequest) bridge.InputResponse {
		return bridge.InputResponse{Action: "accept", Content: map[string]any{"name": "Lin"}}
	}))
	if err != nil || res.Output != "hello Lin" {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := r.Run(ctx, task); err != nil || res.Output != "declined" {
		t.Fatalf("without a handler: %+v %v", res, err)
	}
}
