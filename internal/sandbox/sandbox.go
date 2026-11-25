// Package sandbox runs a runtime, and the commands rbridge runs for it, inside
// an operating system sandbox: bubblewrap on Linux when it works, otherwise
// Docker. In the sandbox only the workspace, the session's scratch folder and
// the runtime's own state folders are writable, and with network off there is
// no network at all, loopback to the host included.
//
// The sandbox limits writes and network. It does not hide files from reads:
// a runtime needs its own sign-in files, which live in the user's home.
package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode is how much a sandbox restricts.
type Mode string

const (
	Off   Mode = "off"
	Files Mode = "files" // writes limited to the workspace and state folders
	Full  Mode = "full"  // Files, and no network
)

// ParseMode accepts off, files or full.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case "", Off:
		return Off, nil
	case Files, Full:
		return Mode(s), nil
	}
	return Off, fmt.Errorf("sandbox must be off, files or full, not %q", s)
}

// Spec is what one session's sandbox allows.
type Spec struct {
	Mode      Mode
	Workspace string
	Scratch   string
	// Writable are extra folders or files the runtime may change, such as
	// its own state folder. Missing folders are created; missing files are
	// left out.
	Writable []string
	// Visible are paths that must be readable even if they live in a folder
	// the sandbox replaces (such as /tmp), for example the runtime binary.
	Visible []string
	// Name labels the sandbox, for example the session id.
	Name string
}

// Engine starts processes inside a sandbox.
type Engine interface {
	Name() string
	// Start prepares a sandbox for a session and returns its launcher and a
	// cleanup function that must run when the session ends.
	Start(ctx context.Context, spec Spec) (*Launcher, func(), error)
}

// Launcher implements domain.Launcher for one session.
type Launcher struct {
	main    func(path string, args, env []string, dir string) (string, []string, []string)
	command func(path string, args, env []string, dir string) (string, []string, []string)
}

func (l *Launcher) Main(path string, args, env []string, dir string) (string, []string, []string) {
	return l.main(path, args, env, dir)
}

func (l *Launcher) Command(path string, args, env []string, dir string) (string, []string, []string) {
	return l.command(path, args, env, dir)
}

// Detect finds a working engine: "auto" tries bubblewrap, then Docker.
// It runs a real probe, since an installed tool may be blocked by the system
// (Ubuntu restricts the user namespaces bubblewrap needs).
func Detect(ctx context.Context, choice string) (Engine, []string) {
	if runtime.GOOS != "linux" && choice != "docker" {
		if choice == "auto" || choice == "" {
			// Docker Desktop runs Linux containers that cannot run host binaries.
			return nil, []string{"sandboxing is available on Linux only"}
		}
	}
	var notes []string
	if choice == "" || choice == "auto" || choice == "bwrap" {
		if err := probeBwrap(ctx); err == nil {
			return bwrap{}, nil
		} else {
			notes = append(notes, "bubblewrap: "+err.Error())
		}
		if choice == "bwrap" {
			return nil, notes
		}
	}
	if choice == "" || choice == "auto" || choice == "docker" {
		if runtime.GOOS != "linux" {
			return nil, append(notes, "docker: host binaries cannot run in containers outside Linux")
		}
		if err := probeDocker(ctx); err == nil {
			return &docker{}, notes
		} else {
			notes = append(notes, "docker: "+err.Error())
		}
	}
	return nil, notes
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	if err != nil {
		msg := strings.TrimSpace(out.String())
		if i := strings.IndexByte(msg, '\n'); i > 0 {
			msg = msg[:i]
		}
		if msg == "" {
			msg = err.Error()
		}
		return out.Bytes(), errors.New(msg)
	}
	return out.Bytes(), nil
}

func probeBwrap(ctx context.Context) error {
	if _, err := exec.LookPath("bwrap"); err != nil {
		return errors.New("not installed")
	}
	_, err := run(ctx, 10*time.Second, "bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--unshare-net", "--", "true")
	return err
}

func probeDocker(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return errors.New("not installed")
	}
	_, err := run(ctx, 15*time.Second, "docker", "version", "--format", "{{.Server.Version}}")
	return err
}

// prepare creates missing writable folders and returns the paths to bind,
// sorted so parents come before children.
func prepare(spec Spec) (writable, visible []string) {
	add := func(list []string, p string) []string {
		if p == "" || slices.Contains(list, p) {
			return list
		}
		return append(list, p)
	}
	writable = add(writable, spec.Workspace)
	writable = add(writable, spec.Scratch)
	for _, w := range spec.Writable {
		if _, err := os.Stat(w); errors.Is(err, os.ErrNotExist) {
			if filepath.Ext(w) != "" {
				continue // a file that does not exist yet
			}
			if err := os.MkdirAll(w, 0o700); err != nil {
				continue
			}
		}
		writable = add(writable, w)
	}
	for _, v := range spec.Visible {
		if _, err := os.Stat(v); err == nil {
			visible = add(visible, v)
		}
	}
	sort.Strings(writable)
	sort.Strings(visible)
	return writable, visible
}

// bwrap uses bubblewrap: the host filesystem read-only, a private /tmp, the
// allowed paths bound read-write, and an empty network namespace for Full.
type bwrap struct{}

func (bwrap) Name() string { return "bubblewrap" }

func (bwrap) Start(_ context.Context, spec Spec) (*Launcher, func(), error) {
	writable, visible := prepare(spec)
	base := []string{"--die-with-parent", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp",
		"--unshare-pid", "--unshare-ipc", "--unshare-uts"}
	if spec.Mode == Full {
		base = append(base, "--unshare-net")
	}
	for _, v := range visible {
		base = append(base, "--ro-bind", v, v)
	}
	for _, w := range writable {
		base = append(base, "--bind", w, w)
	}
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, nil, err
	}
	wrap := func(path string, args, env []string, dir string) (string, []string, []string) {
		full := append(slices.Clone(base), "--chdir", dir, "--", path)
		return bin, append(full, args...), env
	}
	return &Launcher{main: wrap, command: wrap}, func() {}, nil
}

// docker runs the runtime in a container built from an empty image, with the
// host's system folders and the user's home mounted read-only. Commands for
// the runtime run in the same container with docker exec.
type docker struct {
	once  sync.Once
	image string
	err   error
}

func (*docker) Name() string { return "docker" }

const imageName = "rbridge-sandbox:empty"

func (d *docker) ensureImage(ctx context.Context) error {
	d.once.Do(func() {
		if _, err := run(ctx, 15*time.Second, "docker", "image", "inspect", imageName); err == nil {
			d.image = imageName
			return
		}
		// An image with nothing in it: everything comes from read-only mounts.
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		_ = tw.WriteHeader(&tar.Header{Name: "tmp/", Typeflag: tar.TypeDir, Mode: 0o1777})
		_ = tw.Close()
		cmd := exec.CommandContext(ctx, "docker", "import", "-", imageName)
		cmd.Stdin = &buf
		if out, err := cmd.CombinedOutput(); err != nil {
			d.err = fmt.Errorf("creating the sandbox image: %s", strings.TrimSpace(string(out)))
			return
		}
		d.image = imageName
	})
	return d.err
}

func (d *docker) Start(ctx context.Context, spec Spec) (*Launcher, func(), error) {
	if err := d.ensureImage(ctx); err != nil {
		return nil, nil, err
	}
	reapContainers(ctx)
	writable, visible := prepare(spec)
	name := "rbridge-" + strings.ToLower(spec.Name)
	network := "host"
	if spec.Mode == Full {
		network = "none"
	}
	base := []string{"run", "--rm", "-i", "--init", "--name", name,
		"--label", "rbridge.owner-pid=" + strconv.Itoa(os.Getpid()),
		"--network", network, "--read-only", "--tmpfs", "/tmp:rw,exec,mode=1777",
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--security-opt", "no-new-privileges"}
	for _, sys := range []string{"/usr", "/bin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt", "/snap", "/nix"} {
		if _, err := os.Stat(sys); err == nil {
			base = append(base, "-v", sys+":"+sys+":ro")
		}
	}
	for _, v := range visible {
		base = append(base, "-v", v+":"+v+":ro")
	}
	for _, w := range writable {
		base = append(base, "-v", w+":"+w)
	}
	bin, err := exec.LookPath("docker")
	if err != nil {
		return nil, nil, err
	}
	// Values travel in the docker client's environment, never on its
	// command line, where other users could read them.
	envFlags := func(env []string) []string {
		var out []string
		for _, kv := range env {
			if k, _, ok := strings.Cut(kv, "="); ok && k != "" {
				out = append(out, "-e", k)
			}
		}
		return out
	}
	clientEnv := func(env []string) []string {
		out := slices.Clone(env)
		for _, k := range []string{"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "XDG_RUNTIME_DIR"} {
			if v, ok := os.LookupEnv(k); ok {
				out = append(out, k+"="+v)
			}
		}
		if !slices.ContainsFunc(out, func(s string) bool { return strings.HasPrefix(s, "HOME=") }) {
			out = append(out, "HOME="+os.Getenv("HOME"))
		}
		return out
	}
	main := func(path string, args, env []string, dir string) (string, []string, []string) {
		full := append(slices.Clone(base), envFlags(env)...)
		full = append(full, "-w", dir, d.image, path)
		return bin, append(full, args...), clientEnv(env)
	}
	command := func(path string, args, env []string, dir string) (string, []string, []string) {
		full := []string{"exec", "-i", "-w", dir}
		full = append(full, envFlags(env)...)
		full = append(full, name, path)
		return bin, append(full, args...), clientEnv(env)
	}
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = run(ctx, 20*time.Second, "docker", "rm", "-f", name)
	}
	return &Launcher{main: main, command: command}, cleanup, nil
}

// reapContainers removes sandbox containers left by rbridge processes that
// are gone, for example after a crash.
func reapContainers(ctx context.Context) {
	out, err := run(ctx, 15*time.Second, "docker", "ps", "-a", "--filter", "label=rbridge.owner-pid", "--format", `{{.Names}} {{.Label "rbridge.owner-pid"}}`)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		name, pidText, ok := strings.Cut(line, " ")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 || alive(pid) {
			continue
		}
		_, _ = run(ctx, 20*time.Second, "docker", "rm", "-f", name)
	}
}
