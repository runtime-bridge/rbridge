//go:build unix

// Package contract is the behaviour every provider adapter must show. Adapter
// tests run it against a fake runtime that understands these scenarios:
// echo, permission, slow, hang, crash, spawn, auth.
package contract

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/rbridge/rbridge/internal/domain"
)

// Opener starts a session of the adapter under test against its fake runtime
// running the given scenario.
type Opener func(ctx context.Context, t *testing.T, scenario string, opts domain.SessionOptions, sink domain.EventSink) (domain.ProviderSession, error)

// Recorder collects events.
type Recorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *Recorder) Emit(e domain.Event) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

// Events returns a copy of the recorded events.
func (r *Recorder) Events() []domain.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Event(nil), r.events...)
}

// Types returns the event types in order.
func (r *Recorder) Types() []domain.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]domain.EventType, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

// Has reports whether an event of type t was emitted.
func (r *Recorder) Has(t domain.EventType) bool { return slices.Contains(r.Types(), t) }

func baseOpts(t *testing.T) domain.SessionOptions {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	return domain.SessionOptions{
		BridgeID:      "ses_test",
		Workspace:     ws,
		IdleTimeout:   0,
		KillGrace:     time.Second,
		ScratchDir:    t.TempDir(),
		ClientFSWrite: true,
	}
}

func open(t *testing.T, o Opener, scenario string, mutate func(*domain.SessionOptions)) (domain.ProviderSession, *Recorder, domain.SessionOptions) {
	t.Helper()
	opts := baseOpts(t)
	if mutate != nil {
		mutate(&opts)
	}
	rec := &Recorder{}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := o(ctx, t, scenario, opts, rec)
	if err != nil {
		t.Fatalf("open %s: %v", scenario, err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	return s, rec, opts
}

func prompt(t *testing.T, s domain.ProviderSession, text string, timeout time.Duration) (domain.TurnResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.Prompt(ctx, domain.Turn{ID: "turn-1", Prompt: domain.Text(text)})
}

// Run executes the contract suite.
func Run(t *testing.T, o Opener) {
	t.Run("echo", func(t *testing.T) {
		s, rec, _ := open(t, o, "echo", nil)
		if s.NativeID() == "" {
			t.Fatal("empty native session id")
		}
		if len(s.Capabilities()) == 0 {
			t.Fatal("no capabilities")
		}
		res, err := prompt(t, s, "hello there", 20*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != domain.StopEndTurn || !strings.Contains(res.Output, "hello there") {
			t.Fatalf("result %+v", res)
		}
		types := rec.Types()
		if len(types) == 0 || types[0] != domain.EventSessionStarted {
			t.Fatalf("first event should be session.started, got %v", types)
		}
		if !rec.Has(domain.EventMessageDelta) || !rec.Has(domain.EventToolStarted) {
			t.Fatalf("missing message or tool events: %v", types)
		}
		// A second turn works on the same session.
		res, err = prompt(t, s, "again", 20*time.Second)
		if err != nil || !strings.Contains(res.Output, "again") || strings.Contains(res.Output, "hello there") {
			t.Fatalf("second turn: %+v %v", res, err)
		}
	})

	for _, allow := range []bool{true, false} {
		name := "permission-deny"
		if allow {
			name = "permission-allow"
		}
		t.Run(name, func(t *testing.T) {
			var asked domain.ApprovalRequest
			s, rec, opts := open(t, o, "permission", func(op *domain.SessionOptions) {
				op.Approver = domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
					asked = r
					if allow {
						return r.AllowOnce("user")
					}
					return r.RejectOnce("user")
				})
			})
			res, err := prompt(t, s, "write the notes", 20*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if asked.Title == "" || len(asked.Options) == 0 {
				t.Fatalf("approval request incomplete: %+v", asked)
			}
			if !rec.Has(domain.EventApprovalRequired) || !rec.Has(domain.EventApprovalResolved) {
				t.Fatalf("approval events missing: %v", rec.Types())
			}
			_, statErr := os.Stat(filepath.Join(opts.Workspace, "notes.txt"))
			if allow && (statErr != nil || !strings.Contains(res.Output, "allowed")) {
				t.Fatalf("allowed write did not happen: %+v %v", res, statErr)
			}
			if !allow && (statErr == nil || !strings.Contains(res.Output, "denied")) {
				t.Fatalf("denied write happened: %+v %v", res, statErr)
			}
		})
	}

	t.Run("no-approver-denies", func(t *testing.T) {
		s, _, opts := open(t, o, "permission", nil)
		if _, err := prompt(t, s, "write", 20*time.Second); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(opts.Workspace, "notes.txt")); err == nil {
			t.Fatal("write happened without an approver")
		}
	})

	t.Run("cancel", func(t *testing.T) {
		s, _, _ := open(t, o, "slow", nil)
		time.AfterFunc(200*time.Millisecond, func() { s.Cancel(context.Background()) })
		start := time.Now()
		res, err := prompt(t, s, "count", 20*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != domain.StopCancelled {
			t.Fatalf("stop reason %s", res.StopReason)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("cancel took too long")
		}
	})

	t.Run("context-cancel", func(t *testing.T) {
		s, _, _ := open(t, o, "slow", nil)
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		res, err := s.Prompt(ctx, domain.Turn{ID: "t", Prompt: domain.Text("count")})
		if err != nil || res.StopReason != domain.StopCancelled {
			t.Fatalf("got %+v %v", res, err)
		}
	})

	t.Run("idle-watchdog", func(t *testing.T) {
		s, rec, _ := open(t, o, "hang", func(op *domain.SessionOptions) {
			op.IdleTimeout = 300 * time.Millisecond
			op.KillGrace = 300 * time.Millisecond
		})
		start := time.Now()
		_, err := prompt(t, s, "anything", 20*time.Second)
		if domain.CodeOf(err) != domain.ErrExecutionTimeout {
			t.Fatalf("want execution_timeout, got %v", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Fatal("watchdog too slow")
		}
		if !rec.Has(domain.EventNotice) {
			t.Error("watchdog should emit a notice")
		}
	})

	t.Run("crash", func(t *testing.T) {
		s, _, _ := open(t, o, "crash", nil)
		_, err := prompt(t, s, "go", 20*time.Second)
		if domain.CodeOf(err) != domain.ErrExecutionFailed {
			t.Fatalf("want execution_failed, got %v", err)
		}
	})

	t.Run("auth", func(t *testing.T) {
		opts := baseOpts(t)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s, err := o(ctx, t, "auth", opts, &Recorder{})
		if err == nil {
			defer s.Close(context.Background())
			_, err = prompt(t, s, "hi", 20*time.Second)
		}
		if domain.CodeOf(err) != domain.ErrAuthenticationRequired {
			t.Fatalf("want authentication_required, got %v", err)
		}
	})

	t.Run("close-kills-tree", func(t *testing.T) {
		s, _, opts := open(t, o, "spawn", nil)
		if _, err := prompt(t, s, "spawn", 20*time.Second); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(opts.Workspace, "child.pid"))
		if err != nil {
			t.Fatal(err)
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("child %d survived Close", pid)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if _, err := prompt(t, s, "after close", 5*time.Second); err == nil {
			t.Fatal("prompt after close should fail")
		}
	})
}
