package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/process"
)

// terminal is one command rbridge runs for the agent.
type terminal struct {
	cmd     *exec.Cmd
	out     *tailBuffer
	done    chan struct{}
	waitErr error
	timedUp atomic.Bool
	timer   *time.Timer
}

// tailBuffer keeps the last max bytes, as ACP asks clients to truncate from the start.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = b.buf[over:]
		// Do not start in the middle of a UTF-8 character.
		for len(b.buf) > 0 && b.buf[0]&0xC0 == 0x80 {
			b.buf = b.buf[1:]
		}
		b.truncated = true
	}
	return len(p), nil
}

func (b *tailBuffer) snapshot() (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf), b.truncated
}

type terminals struct {
	mu   sync.Mutex
	byID map[string]*terminal
}

func (c *client) terms() *terminals {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()
	if c.s.terminals == nil {
		c.s.terminals = &terminals{byID: map[string]*terminal{}}
	}
	return c.s.terminals
}

func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v
		}
	}
	return ""
}

// lookPath resolves a command against the session's PATH, not rbridge's own.
func lookPath(name string, env []string) (string, error) {
	if strings.ContainsRune(name, filepath.Separator) || strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode().Perm()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

func newTerminalID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "term-" + hex.EncodeToString(b[:])
}

func (c *client) CreateTerminal(ctx context.Context, p sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	s := c.s
	if !s.opts.ClientTerminal {
		return sdk.CreateTerminalResponse{}, errTerminal()
	}
	cwd := s.opts.Workspace
	if p.Cwd != nil && *p.Cwd != "" {
		resolved, err := c.insideWorkspace(*p.Cwd)
		if err != nil {
			return sdk.CreateTerminalResponse{}, err
		}
		cwd = resolved
	}
	if s.opts.CommandGate != nil {
		if err := s.opts.CommandGate(ctx, p.Command, p.Args); err != nil {
			return sdk.CreateTerminalResponse{}, sdk.NewRequestError(-32001, err.Error(), map[string]string{"command": p.Command})
		}
	}
	env := append([]string(nil), s.opts.Env...)
	for _, v := range p.Env {
		env = append(env, v.Name+"="+v.Value)
	}
	path, err := lookPath(p.Command, env)
	if err != nil {
		return sdk.CreateTerminalResponse{}, sdk.NewRequestError(-32002, "command not found: "+p.Command, nil)
	}
	cmdArgs := p.Args
	if s.opts.Launcher != nil {
		path, cmdArgs, env = s.opts.Launcher.Command(path, cmdArgs, env, cwd)
	}
	limit := s.opts.MaxOutputBytes
	if limit <= 0 {
		limit = 1 << 20
	}
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 && *p.OutputByteLimit < limit {
		limit = *p.OutputByteLimit
	}
	out := &tailBuffer{max: int(limit)}
	cmd := exec.Command(path, cmdArgs...)
	cmd.Dir = cwd
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	process.Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return sdk.CreateTerminalResponse{}, sdk.NewInternalError(map[string]string{"error": err.Error()})
	}
	t := &terminal{cmd: cmd, out: out, done: make(chan struct{})}
	go func() {
		t.waitErr = cmd.Wait()
		process.KillGroup(cmd)
		close(t.done)
	}()
	if timeout := s.opts.CommandTimeout; timeout > 0 {
		t.timer = time.AfterFunc(timeout, func() {
			t.timedUp.Store(true)
			process.KillGroup(cmd)
		})
	}
	id := newTerminalID()
	ts := c.terms()
	ts.mu.Lock()
	ts.byID[id] = t
	ts.mu.Unlock()
	s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Level: "info", Message: "runtime started command through rbridge: " + strings.TrimSpace(p.Command+" "+strings.Join(p.Args, " "))}))
	return sdk.CreateTerminalResponse{TerminalId: sdk.TerminalId(id)}, nil
}

func (c *client) terminal(id sdk.TerminalId) (*terminal, error) {
	ts := c.terms()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	t := ts.byID[string(id)]
	if t == nil {
		return nil, sdk.NewInvalidParams(map[string]string{"error": "unknown terminal"})
	}
	return t, nil
}

func (t *terminal) exitStatus() *sdk.TerminalExitStatus {
	select {
	case <-t.done:
	default:
		return nil
	}
	st := &sdk.TerminalExitStatus{}
	if t.timedUp.Load() {
		sig := "SIGKILL"
		st.Signal = &sig
		return st
	}
	var exitErr *exec.ExitError
	if t.waitErr == nil || errors.As(t.waitErr, &exitErr) {
		ps := t.cmd.ProcessState
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			sig := ws.Signal().String()
			st.Signal = &sig
			return st
		}
		code := int64(ps.ExitCode())
		st.ExitCode = &code
	}
	return st
}

func (c *client) TerminalOutput(_ context.Context, p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	t, err := c.terminal(p.TerminalId)
	if err != nil {
		return sdk.TerminalOutputResponse{}, err
	}
	out, truncated := t.out.snapshot()
	return sdk.TerminalOutputResponse{Output: out, Truncated: truncated, ExitStatus: t.exitStatus()}, nil
}

func (c *client) WaitForTerminalExit(ctx context.Context, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	t, err := c.terminal(p.TerminalId)
	if err != nil {
		return sdk.WaitForTerminalExitResponse{}, err
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		return sdk.WaitForTerminalExitResponse{}, ctx.Err()
	}
	st := t.exitStatus()
	return sdk.WaitForTerminalExitResponse{ExitCode: st.ExitCode, Signal: st.Signal}, nil
}

func (c *client) KillTerminal(_ context.Context, p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	t, err := c.terminal(p.TerminalId)
	if err != nil {
		return sdk.KillTerminalResponse{}, err
	}
	t.kill()
	return sdk.KillTerminalResponse{}, nil
}

func (c *client) ReleaseTerminal(_ context.Context, p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	t, err := c.terminal(p.TerminalId)
	if err != nil {
		return sdk.ReleaseTerminalResponse{}, err
	}
	t.kill()
	ts := c.terms()
	ts.mu.Lock()
	delete(ts.byID, string(p.TerminalId))
	ts.mu.Unlock()
	return sdk.ReleaseTerminalResponse{}, nil
}

func (t *terminal) kill() {
	if t.timer != nil {
		t.timer.Stop()
	}
	select {
	case <-t.done:
		return
	default:
	}
	process.KillGroup(t.cmd)
	select {
	case <-t.done:
	case <-time.After(5 * time.Second):
	}
}

// closeAll kills every terminal of the session.
func (ts *terminals) closeAll() {
	if ts == nil {
		return
	}
	ts.mu.Lock()
	all := make([]*terminal, 0, len(ts.byID))
	for id, t := range ts.byID {
		all = append(all, t)
		delete(ts.byID, id)
	}
	ts.mu.Unlock()
	for _, t := range all {
		t.kill()
	}
}
