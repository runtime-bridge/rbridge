//go:build unix

package process

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func sh(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	return p
}

func TestFilterEnv(t *testing.T) {
	env := []string{"PATH=/bin", "HOME=/h", "SECRET_TOKEN=abc", "ANTHROPIC_API_KEY=k", "broken", "LANG=C"}
	got := FilterEnv(env, []string{"PATH", "HOME", "LANG"}, map[string]string{"LANG": "en_US.UTF-8", "EXTRA": "1"})
	slices.Sort(got)
	want := []string{"EXTRA=1", "HOME=/h", "LANG=en_US.UTF-8", "PATH=/bin"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	out, err := Run(context.Background(), Spec{Path: sh(t), Args: []string{"-c", "echo out; echo err >&2; exit 3"}, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Stdout) != "out\n" || string(out.Stderr) != "err\n" || out.ExitCode != 3 {
		t.Fatalf("unexpected output %+v", out)
	}
}

func TestRunUsesOnlyGivenEnv(t *testing.T) {
	t.Setenv("RBRIDGE_TEST_LEAK", "leaked")
	out, err := Run(context.Background(), Spec{Path: sh(t), Args: []string{"-c", "echo \"[$RBRIDGE_TEST_LEAK]\""}, Env: nil, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out.Stdout)) != "[]" {
		t.Fatalf("parent environment leaked: %q", out.Stdout)
	}
}

func TestRunCapsOutput(t *testing.T) {
	out, err := Run(context.Background(), Spec{Path: sh(t), Args: []string{"-c", "i=0; while [ $i -lt 2000 ]; do echo 0123456789; i=$((i+1)); done"}, MaxOutput: 100, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Stdout) != 100 || !out.Truncated {
		t.Fatalf("len=%d truncated=%v", len(out.Stdout), out.Truncated)
	}
}

// TestTimeoutKillsTree starts a shell that spawns a long-lived grandchild and
// ignores SIGTERM, and checks that nothing survives the timeout.
func TestTimeoutKillsTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	script := "trap '' TERM; sleep 60 & echo $! > " + pidFile + "; wait"
	start := time.Now()
	out, err := Run(context.Background(), Spec{Path: sh(t), Args: []string{"-c", script}, Timeout: 300 * time.Millisecond, Grace: 200 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || !out.TimedOut {
		t.Fatalf("want timeout, got %v %+v", err, out)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("stop took too long")
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	deadline := time.Now().Add(2 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestContextCancelStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := Run(ctx, Spec{Path: sh(t), Args: []string{"-c", "sleep 30"}, Grace: 100 * time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestRunMissingBinary(t *testing.T) {
	if _, err := Run(context.Background(), Spec{Path: "/nonexistent/rbridge-test-binary"}); err == nil {
		t.Fatal("expected start error")
	}
}

// Stopping a process must end even when its result never arrives.
func TestStopTreeIsBounded(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	never := make(chan error)
	stopped := make(chan struct{})
	go func() {
		StopTree(cmd, never, 100*time.Millisecond)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("StopTree waited for ever")
	}
}

// A process that exits while a child it detached keeps its output pipe open
// must still be waited for.
func TestWaitIgnoresADetachedChildsPipes(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid here")
	}
	cmd := exec.Command("sh", "-c", "setsid sleep 30 & exit 0")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	Prepare(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Wait blocked on a detached child's pipe")
	}
}
