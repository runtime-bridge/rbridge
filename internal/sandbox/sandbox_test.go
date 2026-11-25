//go:build linux

package sandbox

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Inside the sandbox the test binary acts as a helper that tries to write
// files and open a connection, and reports what happened.
func TestMain(m *testing.M) {
	if os.Getenv("RBRIDGE_SANDBOX_HELPER") == "1" {
		for p := range strings.SplitSeq(os.Getenv("WRITE"), ",") {
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				fmt.Printf("write %s: denied\n", filepath.Base(p))
			} else {
				fmt.Printf("write %s: ok\n", filepath.Base(p))
			}
		}
		if c, err := net.DialTimeout("tcp", os.Getenv("DIAL"), 2*time.Second); err != nil {
			fmt.Println("dial: denied")
		} else {
			c.Close()
			fmt.Println("dial: ok")
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func engine(t *testing.T) Engine {
	t.Helper()
	e, notes := Detect(context.Background(), "auto")
	if e == nil {
		t.Skipf("no sandbox engine here: %v", notes)
	}
	return e
}

func TestSandboxLimits(t *testing.T) {
	e := engine(t)
	t.Logf("engine: %s", e.Name())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	exe, _ := os.Executable()
	ws := t.TempDir()
	scratch := t.TempDir()
	// Outside the workspace and outside /tmp, which the sandbox replaces.
	outside, err := os.MkdirTemp(".", "outside-")
	if err != nil {
		t.Fatal(err)
	}
	outside, _ = filepath.Abs(outside)
	defer os.RemoveAll(outside)

	for _, mode := range []Mode{Files, Full} {
		t.Run(string(mode), func(t *testing.T) {
			l, cleanup, err := e.Start(context.Background(), Spec{
				Mode: mode, Workspace: ws, Scratch: scratch, Visible: []string{filepath.Dir(exe)},
				Name: "test-" + string(mode) + fmt.Sprint(time.Now().UnixNano()),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			env := []string{"PATH=/usr/bin:/bin", "HOME=" + os.Getenv("HOME"), "RBRIDGE_SANDBOX_HELPER=1",
				"WRITE=" + filepath.Join(ws, "inside.txt") + "," + filepath.Join(outside, "outside.txt"),
				"DIAL=" + ln.Addr().String()}
			path, args, cenv := l.Main(exe, nil, env, ws)
			cmd := exec.Command(path, args...)
			cmd.Env = cenv
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			got := string(out)
			if !strings.Contains(got, "write inside.txt: ok") || !strings.Contains(got, "write outside.txt: denied") {
				t.Fatalf("file limits: %s", got)
			}
			if _, err := os.Stat(filepath.Join(outside, "outside.txt")); err == nil {
				t.Fatal("a file outside the workspace was written")
			}
			wantDial := "dial: ok"
			if mode == Full {
				wantDial = "dial: denied"
			}
			if !strings.Contains(got, wantDial) {
				t.Fatalf("network in %s mode: %s", mode, got)
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "files": Files, "full": Full} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Errorf("%q: %s %v", in, got, err)
		}
	}
	if _, err := ParseMode("strict"); err == nil {
		t.Error("unknown mode accepted")
	}
}
