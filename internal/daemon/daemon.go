package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/buildinfo"
	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
)

// State is written to daemon.json while the daemon runs.
type State struct {
	PID     int       `json:"pid"`
	Version string    `json:"version"`
	Started time.Time `json:"started_at"`
	Socket  string    `json:"socket,omitempty"`
	HTTP    string    `json:"http,omitempty"`
}

// Files are the daemon's files in the runtime directory.
type Files struct {
	Dir, Socket, Token, State, Log string
}

// maxSocketPath stays below the smallest sun_path limit (104 bytes on macOS).
const maxSocketPath = 100

// FilesFor returns the file locations for a runtime directory. When the
// socket path would be too long for a Unix socket, the socket goes into a
// short private directory under the system temp dir, derived from the runtime
// directory; daemon.json records where it is.
func FilesFor(p config.Paths) Files {
	d := p.RuntimeDir
	sock := filepath.Join(d, "bridge.sock")
	if len(sock) > maxSocketPath {
		sum := sha256.Sum256([]byte(d))
		sock = filepath.Join(os.TempDir(), fmt.Sprintf("rbridge-%d-%x", os.Getuid(), sum[:6]), "bridge.sock")
	}
	return Files{Dir: d, Socket: sock, Token: filepath.Join(d, "daemon.token"), State: filepath.Join(d, "daemon.json"), Log: filepath.Join(d, "daemon.log")}
}

// SocketFor returns the socket of the running daemon from daemon.json, or the
// computed path when no daemon has written one.
func SocketFor(f Files) string {
	if st, err := ReadState(f); err == nil && st.Socket != "" {
		return st.Socket
	}
	return f.Socket
}

// ErrAlreadyRunning means another daemon answers on the socket.
var ErrAlreadyRunning = errors.New("an rbridge daemon is already running")

// ReadState returns the running daemon's state.
func ReadState(f Files) (State, error) {
	var s State
	b, err := os.ReadFile(f.State)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// ReadToken returns the HTTP token of the running daemon.
func ReadToken(f Files) (string, error) {
	b, err := os.ReadFile(f.Token)
	return string(b), err
}

// Options configure Run.
type Options struct {
	Service *service.Service
	Config  config.Config
	Files   Files
	Logger  *slog.Logger
	// Ready, if set, is called once the listeners are up.
	Ready func(State)
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}
	f := o.Files
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(f.Dir, 0o700)
	if sockDir := filepath.Dir(f.Socket); sockDir != f.Dir {
		if err := os.MkdirAll(sockDir, 0o700); err != nil {
			return err
		}
		// The directory must be ours and private, or another user could take the socket.
		if fi, err := os.Lstat(sockDir); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			return fmt.Errorf("socket directory %s is not a private directory", sockDir)
		}
	}
	if c, err := net.DialTimeout("unix", f.Socket, time.Second); err == nil {
		c.Close()
		return ErrAlreadyRunning
	}
	_ = os.Remove(f.Socket)

	hub := NewHub(o.Service, o.Config.Daemon.EventBuffer, o.Config.Daemon.SessionIdleTimeout)
	api := &API{Hub: hub, Svc: o.Service, Started: time.Now().UTC()}
	if list, err := o.Service.Sessions(ctx); err == nil {
		n := 0
		for _, r := range list {
			if r.State == domain.StateInterrupted {
				n++
			}
		}
		if n > 0 {
			log.Info("sessions left over from a previous run are marked interrupted", "count", n)
		}
	}

	state := State{PID: os.Getpid(), Version: buildinfo.Get().Version, Started: api.Started}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	var closers []func()

	if o.Config.Daemon.IPC.Enabled {
		l, err := net.Listen("unix", f.Socket)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", f.Socket, err)
		}
		_ = os.Chmod(f.Socket, 0o600)
		state.Socket = f.Socket
		closers = append(closers, func() { l.Close() })
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ServeIPC(ctx, l, api); err != nil {
				errs <- err
			}
		}()
	}

	var srv *http.Server
	if hc := o.Config.Daemon.HTTP; hc.Enabled {
		ip := net.ParseIP(hc.Host)
		if (ip == nil || !ip.IsLoopback()) && hc.Host != "localhost" && !hc.AllowNonLoopback {
			return fmt.Errorf("daemon.http.host %q is not a loopback address; set daemon.http.allow_non_loopback to listen there", hc.Host)
		}
		token, err := newToken()
		if err != nil {
			return err
		}
		if err := os.WriteFile(f.Token, []byte(token), 0o600); err != nil {
			return err
		}
		l, err := net.Listen("tcp", net.JoinHostPort(hc.Host, strconv.Itoa(hc.Port)))
		if err != nil {
			return fmt.Errorf("listen on %s:%d: %w", hc.Host, hc.Port, err)
		}
		state.HTTP = "http://" + l.Addr().String()
		srv = &http.Server{
			Handler: NewHTTPHandler(api, HTTPOptions{
				Token: token, AllowedOrigins: hc.AllowedOrigins, CheckHost: !hc.AllowNonLoopback, RequestsPerSecond: o.Config.Daemon.RequestsPerSecond,
			}),
			ReadHeaderTimeout: 10 * time.Second,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}()
	}
	if state.Socket == "" && state.HTTP == "" {
		return errors.New("neither daemon.ipc nor daemon.http is enabled")
	}

	b, _ := json.MarshalIndent(state, "", "  ")
	if err := os.WriteFile(f.State, b, 0o600); err != nil {
		return err
	}
	log.Info("rbridge daemon started", "pid", state.PID, "socket", state.Socket, "http", state.HTTP)
	if o.Ready != nil {
		o.Ready(state)
	}

	reap := time.NewTicker(time.Minute)
	defer reap.Stop()
	var runErr error
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errs:
			runErr = err
			break loop
		case <-reap.C:
			if n := hub.Reap(context.Background()); n > 0 {
				log.Info("closed idle sessions", "count", n)
			}
		}
	}

	log.Info("rbridge daemon stopping")
	sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	hub.Shutdown(sctx)
	if srv != nil {
		_ = srv.Shutdown(sctx)
	}
	for _, c := range closers {
		c()
	}
	wg.Wait()
	os.Remove(f.Socket)
	if sockDir := filepath.Dir(f.Socket); sockDir != f.Dir {
		os.Remove(sockDir)
	}
	os.Remove(f.State)
	os.Remove(f.Token)
	return runErr
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
