package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rbridge/rbridge/internal/domain"
	"github.com/rbridge/rbridge/internal/service"
)

// listFlag collects a repeatable flag.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }
func (l *listFlag) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

type taskFlags struct {
	workspace string
	provider  string
	model     string
	approve   string
	schema    string
	timeout   time.Duration
	asJSON    bool
	quiet     bool
	skills    listFlag
	requires  listFlag
}

func (f *taskFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.workspace, "workspace", "", "project directory the runtime works in (default: current directory)")
	fs.StringVar(&f.provider, "provider", "", "provider id; default is the first available one that meets the requirements")
	fs.StringVar(&f.model, "model", "", "model name passed to the runtime")
	fs.StringVar(&f.approve, "approve", "", "how to answer permission requests: ask, allow or deny (default ask on a terminal, otherwise deny)")
	fs.StringVar(&f.schema, "schema", "", "JSON schema file for structured output")
	fs.DurationVar(&f.timeout, "timeout", 0, "task timeout (default from config)")
	fs.BoolVar(&f.asJSON, "json", false, "print events and the result as JSON lines")
	fs.BoolVar(&f.quiet, "quiet", false, "print only the final answer")
	fs.Var(&f.skills, "skill", "skill id to deliver; repeatable")
	fs.Var(&f.requires, "require", "required capability; repeatable")
}

func (f *taskFlags) task(env Env, prompt string) (domain.Task, error) {
	ws := f.workspace
	if ws == "" {
		ws = env.Dir
	}
	ws, err := filepath.Abs(ws)
	if err != nil {
		return domain.Task{}, err
	}
	t := domain.Task{Workspace: ws, Prompt: domain.Text(prompt), Skills: f.skills, Provider: f.provider, Timeout: f.timeout}
	for _, r := range f.requires {
		t.Requirements = append(t.Requirements, domain.CapabilityName(r))
	}
	if f.schema != "" {
		b, err := os.ReadFile(f.schema)
		if err != nil {
			return t, domain.Errorf(domain.ErrInvalidInput, "read schema: %v", err)
		}
		if !json.Valid(b) {
			return t, domain.Errorf(domain.ErrInvalidInput, "schema %s is not valid JSON", f.schema)
		}
		t.OutputSchema = b
	}
	return t, nil
}

// printer renders events for people or as JSON lines.
type printer struct {
	env    Env
	asJSON bool
	quiet  bool
	mu     sync.Mutex
	midMsg bool
}

func (p *printer) Emit(e domain.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.asJSON {
		e.Raw = nil
		b, _ := json.Marshal(e)
		fmt.Fprintln(p.env.Stdout, string(b))
		return
	}
	if p.quiet {
		return
	}
	switch e.Type {
	case domain.EventMessageDelta:
		var d domain.TextData
		json.Unmarshal(e.Data, &d)
		fmt.Fprint(p.env.Stdout, d.Text)
		p.midMsg = true
		return
	case domain.EventToolStarted:
		var d domain.ToolData
		json.Unmarshal(e.Data, &d)
		p.line("[tool] %s", d.Title)
	case domain.EventToolUpdated:
		var d domain.ToolData
		json.Unmarshal(e.Data, &d)
		if d.Status == "failed" {
			p.line("[tool] %s failed", d.ID)
		}
	case domain.EventNotice:
		var d domain.NoticeData
		json.Unmarshal(e.Data, &d)
		if d.Level == "warn" {
			p.line("[warn] %s", d.Message)
		}
	case domain.EventSkillDelivered:
		var d domain.SkillDeliveredData
		json.Unmarshal(e.Data, &d)
		scripts := "with scripts"
		if !d.Scripts {
			scripts = "without scripts"
		}
		p.line("[skill] %s delivered (%s, %s)", d.SkillID, d.Mode, scripts)
	case domain.EventSessionStarted:
		var d domain.SessionStartedData
		json.Unmarshal(e.Data, &d)
		model := ""
		if d.Model != "" {
			model = ", model " + d.Model
		}
		p.line("[session] %s on %s%s", e.SessionID, e.Provider, model)
	}
}

// line prints a status line on stderr, starting a new line if a message is in progress.
func (p *printer) line(format string, args ...any) {
	if p.midMsg {
		fmt.Fprintln(p.env.Stdout)
		p.midMsg = false
	}
	fmt.Fprintf(p.env.Stderr, format+"\n", args...)
}

func (p *printer) result(r domain.Result) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.asJSON {
		b, _ := json.Marshal(map[string]any{"type": "result", "result": r})
		fmt.Fprintln(p.env.Stdout, string(b))
		return
	}
	if p.quiet {
		fmt.Fprintln(p.env.Stdout, r.Output)
		return
	}
	if p.midMsg {
		fmt.Fprintln(p.env.Stdout)
		p.midMsg = false
	} else if r.Output != "" {
		fmt.Fprintln(p.env.Stdout, r.Output)
	}
	usage := ""
	if r.Usage != nil {
		usage = fmt.Sprintf(", %d in / %d out tokens", r.Usage.InputTokens, r.Usage.OutputTokens)
	}
	fmt.Fprintf(p.env.Stderr, "[done] %s, stop %s%s\n", r.SessionID, r.StopReason, usage)
}

// approver answers permission requests according to --approve.
func newApprover(mode string, env Env, in *bufio.Reader) (domain.Approver, error) {
	if mode == "" {
		mode = "deny"
		if env.StdinTTY {
			mode = "ask"
		}
	}
	switch mode {
	case "allow":
		return domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
			fmt.Fprintf(env.Stderr, "[approve] allowed %s\n", r.Title)
			return r.AllowOnce("user")
		}), nil
	case "deny":
		return domain.ApproverFunc(func(_ context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
			fmt.Fprintf(env.Stderr, "[approve] denied %s (use --approve ask or allow)\n", r.Title)
			d := r.RejectOnce("user")
			d.Reason = "denied by --approve deny"
			return d
		}), nil
	case "ask":
		var mu sync.Mutex
		always := map[string]bool{}
		return domain.ApproverFunc(func(ctx context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
			mu.Lock()
			defer mu.Unlock()
			key := r.Tool + "|" + r.Kind
			if always[key] {
				return r.AllowOnce("user")
			}
			fmt.Fprintf(env.Stderr, "\n[approve] %s wants to: %s\n          allow? [y]es / [n]o / [a]lways this session: ", r.Tool, r.Title)
			answer := make(chan string, 1)
			go func() {
				s, _ := in.ReadString('\n')
				answer <- strings.ToLower(strings.TrimSpace(s))
			}()
			select {
			case a := <-answer:
				switch a {
				case "y", "yes":
					return r.AllowOnce("user")
				case "a", "always":
					always[key] = true
					return r.AllowOnce("user")
				}
				d := r.RejectOnce("user")
				d.Reason = "denied by the user"
				return d
			case <-ctx.Done():
				d := r.RejectOnce("timeout")
				d.Reason = "no answer"
				return d
			}
		}), nil
	}
	return nil, domain.Errorf(domain.ErrInvalidInput, "--approve must be ask, allow or deny")
}

func cmdRun(ctx context.Context, args []string, env Env) error {
	fs := newFlags("run", env)
	var tf taskFlags
	tf.register(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(pos, " "))
	in := bufio.NewReader(env.Stdin)
	if prompt == "" || prompt == "-" {
		b, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge run [flags] \"<prompt>\" (or pipe the prompt on stdin)")
	}
	t, err := tf.task(env, prompt)
	if err != nil {
		return err
	}
	approver, err := newApprover(tf.approve, env, in)
	if err != nil {
		return err
	}
	svc, err := newServiceFor(env, t.Workspace)
	if err != nil {
		return err
	}
	pr := &printer{env: env, asJSON: tf.asJSON, quiet: tf.quiet}
	res, err := svc.Run(ctx, t, service.RunOptions{Sink: pr, Approver: approver, Model: tf.model, Client: "cli"})
	if err != nil {
		if tf.asJSON {
			b, _ := json.Marshal(map[string]any{"type": "error", "error": err})
			fmt.Fprintln(env.Stdout, string(b))
		}
		return err
	}
	pr.result(res)
	if res.StopReason == domain.StopCancelled {
		return errSilent{code: domain.ErrCancelled.ExitCode()}
	}
	return nil
}

func cmdChat(ctx context.Context, args []string, env Env) error {
	fs := newFlags("chat", env)
	var tf taskFlags
	tf.register(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	t, err := tf.task(env, "chat")
	if err != nil {
		return err
	}
	in := bufio.NewReader(env.Stdin)
	approver, err := newApprover(tf.approve, env, in)
	if err != nil {
		return err
	}
	svc, err := newServiceFor(env, t.Workspace)
	if err != nil {
		return err
	}
	pr := &printer{env: env, asJSON: tf.asJSON, quiet: tf.quiet}
	sess, err := svc.OpenSession(ctx, t, service.RunOptions{Sink: pr, Approver: approver, Model: tf.model, Client: "cli"})
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())
	return chatLoop(ctx, sess, strings.Join(pos, " "), in, pr, env)
}

func chatLoop(ctx context.Context, sess *service.Session, first string, in *bufio.Reader, pr *printer, env Env) error {
	if !pr.asJSON {
		fmt.Fprintf(env.Stderr, "[chat] session %s; type /exit to leave\n", sess.Record().ID)
	}
	next := strings.TrimSpace(first)
	for {
		if next == "" {
			if !pr.asJSON {
				fmt.Fprint(env.Stderr, "> ")
			}
			line, err := in.ReadString('\n')
			if err != nil && line == "" {
				return nil
			}
			next = strings.TrimSpace(line)
			if next == "" {
				continue
			}
		}
		if next == "/exit" || next == "/quit" {
			return nil
		}
		res, err := sess.Prompt(ctx, domain.Text(next))
		next = ""
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			fmt.Fprintln(env.Stderr, "rbridge:", err)
			continue
		}
		pr.result(res)
		if ctx.Err() != nil {
			return nil
		}
	}
}
