// Package process starts runtime subprocesses with a filtered environment and
// makes sure the whole process tree can be stopped.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// FilterEnv keeps only the variables named in allow, then adds extra.
// Names compare case-insensitively on Windows, where the OS does the same.
func FilterEnv(environ []string, allow []string, extra map[string]string) []string {
	fold := runtime.GOOS == "windows"
	key := func(s string) string {
		if fold {
			return strings.ToUpper(s)
		}
		return s
	}
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[key(a)] = true
	}
	overridden := make(map[string]bool, len(extra))
	for k := range extra {
		overridden[key(k)] = true
	}
	out := make([]string, 0, len(allow)+len(extra))
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			continue
		}
		if allowed[key(name)] && !overridden[key(name)] {
			out = append(out, kv)
		}
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Spec describes a command to run to completion.
type Spec struct {
	Path    string
	Args    []string
	Dir     string
	Env     []string // nil means an empty environment, never the parent's
	Timeout time.Duration
	// MaxOutput caps each of stdout and stderr. Extra output is dropped.
	MaxOutput int64
	// Grace is how long to wait between the polite stop and the kill.
	Grace time.Duration
}

// Output is the result of Run.
type Output struct {
	Stdout    []byte
	Stderr    []byte
	ExitCode  int
	Truncated bool
	TimedOut  bool
}

// ErrTimeout is returned by Run when the command exceeded its timeout.
var ErrTimeout = errors.New("process timed out")

// Run starts the command, waits for it, and stops the whole tree on timeout
// or context cancellation.
func Run(ctx context.Context, s Spec) (Output, error) {
	if s.Path == "" {
		return Output{}, errors.New("process: empty path")
	}
	if s.MaxOutput <= 0 {
		s.MaxOutput = 1 << 20
	}
	if s.Grace <= 0 {
		s.Grace = 2 * time.Second
	}
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	stdout := &capped{max: s.MaxOutput}
	stderr := &capped{max: s.MaxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		return Output{}, fmt.Errorf("start %s: %w", s.Path, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var timeout <-chan time.Time
	if s.Timeout > 0 {
		timer := time.NewTimer(s.Timeout)
		defer timer.Stop()
		timeout = timer.C
	}

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-timeout:
		timedOut = true
		waitErr = StopTree(cmd, done, s.Grace)
	case <-ctx.Done():
		waitErr = StopTree(cmd, done, s.Grace)
	}

	out := Output{
		Stdout:    stdout.Bytes(),
		Stderr:    stderr.Bytes(),
		ExitCode:  -1,
		Truncated: stdout.truncated || stderr.truncated,
		TimedOut:  timedOut,
	}
	if cmd.ProcessState != nil {
		out.ExitCode = cmd.ProcessState.ExitCode()
	}
	switch {
	case timedOut:
		return out, ErrTimeout
	case ctx.Err() != nil:
		return out, ctx.Err()
	}
	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		return out, nil
	}
	return out, waitErr
}

// waitDelay bounds how long Wait waits, once a process has exited, for output
// pipes that a child it detached (its own session, beyond the tree kill)
// keeps open.
const waitDelay = 5 * time.Second

// StopTree asks the process tree to stop, waits up to grace, then kills it.
// done must deliver the result of cmd.Wait.
func StopTree(cmd *exec.Cmd, done <-chan error, grace time.Duration) error {
	if cmd.Process == nil {
		return errors.New("process: not started")
	}
	terminateTree(cmd)
	select {
	case err := <-done:
		killTree(cmd) // descendants may outlive the leader
		return err
	case <-time.After(grace):
	}
	killTree(cmd)
	// A killed process is reaped at once; if its result still does not come
	// (something else holds what Wait waits for), stop waiting rather than
	// hold up every close and shutdown behind it.
	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		return errors.New("process: no exit status after kill")
	}
}

// KillGroup kills whatever is left of the process tree after the leader exited.
func KillGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		killTree(cmd)
	}
}

// capped is a buffer that keeps at most max bytes.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int64
	truncated bool
}

func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.max - int64(c.buf.Len())
	if room <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if int64(len(p)) > room {
		c.buf.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func (c *capped) Bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}
