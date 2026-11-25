// Package cli implements the rbridge command line on top of the service layer.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/runtime-bridge/rbridge/internal/buildinfo"
	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
)

// Env is the process environment the CLI runs in. Tests supply their own.
type Env struct {
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Getenv  func(string) string
	Environ func() []string
	Home    string
	Dir     string
	GOOS    string
	UID     int
	// StdinTTY says whether stdin is a terminal, so approvals can be asked.
	StdinTTY bool
}

// OSEnv returns the environment of the running process.
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	dir, _ := os.Getwd()
	tty := false
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		tty = true
	}
	return Env{
		Stdin: os.Stdin, StdinTTY: tty,
		Stdout: os.Stdout, Stderr: os.Stderr,
		Getenv: os.Getenv, Environ: os.Environ,
		Home: home, Dir: dir, GOOS: runtime.GOOS, UID: os.Getuid(),
	}
}

const usage = `rbridge connects your software to the agent runtimes installed on this machine.

Usage:
  rbridge providers [--refresh] [--json]       List runtimes and their status
  rbridge providers show <id>                  Capabilities, skill locations and limits
  rbridge doctor [--json]                      Check configuration, directories and runtimes
  rbridge config [--defaults]                  Print the effective configuration and its layers

  rbridge run [flags] "<prompt>"               Run one task and print the answer
  rbridge chat [flags] ["<first prompt>"]      Talk to a runtime in a session
      --workspace DIR   --provider ID   --skill ID   --require CAP   --model NAME
      --profile observe|standard|controlled|custom   --approve ask|allow|deny
      --timeout 10m   --schema FILE   --json   --quiet

  rbridge sessions [list|show|resume|cancel]   Manage sessions
  rbridge skills list|add|show|trust|verify|remove|deliver
                                               Manage the skill library
  rbridge audit [--since 24h] [--json]         One line per session: profile, approvals, gaps
  rbridge daemon start|stop|status|run         Optional daemon serving IPC and local HTTP
  rbridge mcp [--profile observe]              MCP server on stdio for other agents

  rbridge version [--json]                     Print version information
  rbridge help                                 Show this help

Environment:
  RBRIDGE_HOME   Keep all rbridge state under one directory
`

// Run executes one command line and returns the exit code.
func Run(ctx context.Context, args []string, env Env) int {
	if env.Stdin == nil {
		env.Stdin = strings.NewReader("")
	}
	if len(args) == 0 {
		fmt.Fprint(env.Stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(env.Stdout, usage)
		return 0
	case "version", "--version":
		err = cmdVersion(args[1:], env)
	case "providers":
		err = cmdProviders(ctx, args[1:], env)
	case "doctor":
		err = cmdDoctor(ctx, args[1:], env)
	case "run":
		err = cmdRun(ctx, args[1:], env)
	case "chat":
		err = cmdChat(ctx, args[1:], env)
	case "sessions":
		err = cmdSessions(ctx, args[1:], env)
	case "skills":
		err = cmdSkills(ctx, args[1:], env)
	case "audit":
		err = cmdAudit(ctx, args[1:], env)
	case "daemon":
		err = cmdDaemon(ctx, args[1:], env)
	case "mcp":
		err = cmdMCP(ctx, args[1:], env)
	case "config":
		err = cmdConfig(ctx, args[1:], env)
	default:
		err = domain.Errorf(domain.ErrInvalidInput, "unknown command %q; run rbridge help", args[0])
	}
	return report(err, env)
}

// errSilent marks an error that has already been reported to the user.
type errSilent struct{ code int }

func (e errSilent) Error() string { return "already reported" }

func report(err error, env Env) int {
	if err == nil {
		return 0
	}
	var s errSilent
	if errors.As(err, &s) {
		return s.code
	}
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintln(env.Stderr, "rbridge:", err)
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code.ExitCode()
	}
	return domain.ErrInternal.ExitCode()
}

func newFlags(name string, env Env) *flag.FlagSet {
	fs := flag.NewFlagSet("rbridge "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	return fs
}

// parse accepts flags before and after positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, domain.Errorf(domain.ErrInvalidInput, "%v", err)
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func newService(env Env) (*service.Service, error) { return newServiceFor(env, env.Dir) }

// newServiceFor loads project configuration from the given workspace.
func newServiceFor(env Env, workspace string) (*service.Service, error) {
	return service.FromEnvironment(service.Environment{
		Env:        config.Env{GOOS: env.GOOS, Getenv: env.Getenv, HomeDir: env.Home, UID: env.UID},
		Environ:    env.Environ,
		ProjectDir: workspace,
	})
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func cmdVersion(args []string, env Env) error {
	fs := newFlags("version", env)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	info := buildinfo.Get()
	if *asJSON {
		return writeJSON(env.Stdout, info)
	}
	line := "rbridge " + info.Version
	if info.Commit != "" {
		c := info.Commit
		if len(c) > 12 {
			c = c[:12]
		}
		if info.Modified {
			c += "+dirty"
		}
		line += " (" + c + ")"
	}
	fmt.Fprintf(env.Stdout, "%s %s %s\n", line, info.GoVersion, info.Platform)
	return nil
}

func cmdProviders(ctx context.Context, args []string, env Env) error {
	if len(args) > 0 && args[0] == "show" {
		return cmdProvidersShow(ctx, args[1:], env)
	}
	fs := newFlags("providers", env)
	refresh := fs.Bool("refresh", false, "detect again instead of using the cache")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return domain.Errorf(domain.ErrInvalidInput, "unexpected argument %q", pos[0])
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	list, err := svc.Providers(ctx, *refresh)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, list)
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSTATUS\tVERSION\tINTEGRATION\tAUTH\tPATH")
	for _, p := range list {
		rt := p.Runtime
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.ID, rt.Health, dash(rt.Version), rt.Integration, rt.AuthMode, dash(rt.Executable))
	}
	return tw.Flush()
}

func cmdProvidersShow(ctx context.Context, args []string, env Env) error {
	fs := newFlags("providers show", env)
	refresh := fs.Bool("refresh", false, "detect again instead of using the cache")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge providers show <id>")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	p, err := svc.Provider(ctx, pos[0], *refresh)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, p)
	}
	w := env.Stdout
	rt := p.Runtime
	fmt.Fprintf(w, "%s (%s)\n", p.DisplayName, p.ID)
	fmt.Fprintf(w, "  status:       %s\n", rt.Health)
	fmt.Fprintf(w, "  version:      %s\n", dash(rt.Version))
	fmt.Fprintf(w, "  executable:   %s%s\n", dash(rt.Executable), pinned(rt.Pinned))
	fmt.Fprintf(w, "  integration:  %s\n", rt.Integration)
	fmt.Fprintf(w, "  auth:         %s\n", rt.AuthMode)
	fmt.Fprintf(w, "  class:        %s\n", p.Class)
	if len(rt.Installations) > 1 || hasErrors(rt.Installations) {
		fmt.Fprintln(w, "\nInstallations:")
		for _, in := range rt.Installations {
			fmt.Fprintf(w, "  %s  %s  %s\n", in.Path, dash(in.Version), in.Error)
		}
	}
	fmt.Fprintln(w, "\nCapabilities:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if len(p.Capabilities) == 0 {
		fmt.Fprintln(tw, "  all unknown until the runtime reports them")
	}
	for _, n := range p.Capabilities.Names() {
		c := p.Capabilities[n]
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", n, c.Supported, c.Source, c.Mode)
	}
	tw.Flush()
	fmt.Fprintln(w, "\nSkill locations:")
	fmt.Fprintf(w, "  project:      %s\n", joinOr(p.Skills.Project, "none"))
	fmt.Fprintf(w, "  user:         %s\n", joinOr(p.Skills.User, "none"))
	if p.Skills.SessionDir != "" {
		fmt.Fprintf(w, "  per session:  <session dir>/%s\n", p.Skills.SessionDir)
	}
	printList(w, "Limits", p.Limits)
	printList(w, "Notes", p.Notes)
	return nil
}

func cmdDoctor(ctx context.Context, args []string, env Env) error {
	fs := newFlags("doctor", env)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	r := svc.Doctor(ctx)
	if *asJSON {
		if err := writeJSON(env.Stdout, r); err != nil {
			return err
		}
	} else {
		for _, c := range r.Checks {
			fmt.Fprintf(env.Stdout, "[%s] %s: %s\n", strings.ToUpper(string(c.Status)), c.Name, c.Detail)
			if c.Fix != "" {
				fmt.Fprintf(env.Stdout, "       fix: %s\n", c.Fix)
			}
		}
	}
	if r.Failed() {
		return errSilent{code: 1}
	}
	return nil
}

func printList(w io.Writer, title string, items []string) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintf(w, "\n%s:\n", title)
	for _, it := range items {
		fmt.Fprintf(w, "  - %s\n", it)
	}
}

func hasErrors(in []domain.Installation) bool {
	for _, i := range in {
		if i.Error != "" {
			return true
		}
	}
	return false
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func pinned(p bool) string {
	if p {
		return " (pinned)"
	}
	return ""
}

func joinOr(items []string, empty string) string {
	if len(items) == 0 {
		return empty
	}
	return strings.Join(items, ", ")
}
