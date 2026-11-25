package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/runtime-bridge/rbridge/internal/daemon"
	"github.com/runtime-bridge/rbridge/internal/domain"
)

func cmdDaemon(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge daemon start|stop|status|run")
	}
	switch args[0] {
	case "run":
		return daemonRun(ctx, env)
	case "start":
		return daemonStart(ctx, env)
	case "stop":
		return daemonStop(env)
	case "status":
		return daemonStatus(env)
	}
	return domain.Errorf(domain.ErrInvalidInput, "unknown daemon command %q (start, stop, status, run)", args[0])
}

func daemonFiles(env Env) (daemon.Files, error) {
	svc, err := newService(env)
	if err != nil {
		return daemon.Files{}, err
	}
	return daemon.FilesFor(svc.Paths()), nil
}

func daemonRun(ctx context.Context, env Env) error {
	svc, err := newService(env)
	if err != nil {
		return err
	}
	f := daemon.FilesFor(svc.Paths())
	logger := slog.New(slog.NewTextHandler(env.Stderr, nil))
	err = daemon.Run(ctx, daemon.Options{Service: svc, Config: svc.Config(), Files: f, Logger: logger})
	if errors.Is(err, daemon.ErrAlreadyRunning) {
		return domain.Errorf(domain.ErrInvalidInput, "%v", err)
	}
	return err
}

func logTail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "no log"
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func alive(f daemon.Files) bool {
	c, err := net.DialTimeout("unix", daemon.SocketFor(f), time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func daemonStart(ctx context.Context, env Env) error {
	f, err := daemonFiles(env)
	if err != nil {
		return err
	}
	if alive(f) {
		st, _ := daemon.ReadState(f)
		fmt.Fprintf(env.Stdout, "already running (pid %d)\n", st.PID)
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(f.Log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon", "run")
	cmd.Env = env.Environ()
	cmd.Dir = env.Home
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-exited:
			return domain.Errorf(domain.ErrExecutionFailed, "the daemon exited during start: %s (log: %s)", logTail(f.Log), f.Log)
		case <-deadline:
			return domain.Errorf(domain.ErrExecutionTimeout, "the daemon did not start within 15s; see %s", f.Log)
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if st, err := daemon.ReadState(f); err == nil && st.PID == cmd.Process.Pid && alive(f) {
			fmt.Fprintf(env.Stdout, "started rbridge daemon (pid %d)\n  socket: %s\n", st.PID, st.Socket)
			if st.HTTP != "" {
				fmt.Fprintf(env.Stdout, "  http:   %s (token in %s)\n", st.HTTP, f.Token)
			}
			fmt.Fprintf(env.Stdout, "  log:    %s\n", f.Log)
			_ = cmd.Process.Release()
			return nil
		}
	}
}

func daemonStop(env Env) error {
	f, err := daemonFiles(env)
	if err != nil {
		return err
	}
	st, err := daemon.ReadState(f)
	if err != nil || !alive(f) {
		fmt.Fprintln(env.Stdout, "not running")
		return nil
	}
	if err := terminate(st.PID); err != nil {
		return err
	}
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(f.State); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(env.Stdout, "stopped rbridge daemon (pid %d)\n", st.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return domain.Errorf(domain.ErrExecutionTimeout, "daemon pid %d did not stop within 20s", st.PID)
}

func daemonStatus(env Env) error {
	f, err := daemonFiles(env)
	if err != nil {
		return err
	}
	st, err := daemon.ReadState(f)
	if err != nil || !alive(f) {
		fmt.Fprintln(env.Stdout, "not running")
		return errSilent{code: 1}
	}
	fmt.Fprintf(env.Stdout, "running (pid %d, version %s, since %s)\n  socket: %s\n", st.PID, st.Version, st.Started.Local().Format("2006-01-02 15:04:05"), st.Socket)
	if st.HTTP != "" {
		fmt.Fprintf(env.Stdout, "  http:   %s\n", st.HTTP)
	}
	return nil
}
