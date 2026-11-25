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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
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
	profile   string
	model     string
	mode      string
	sandbox   string
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
	fs.StringVar(&f.sandbox, "sandbox", "", "run the runtime in an OS sandbox: off, files (writes limited to the workspace) or full (and no network); default from config")
	fs.StringVar(&f.mode, "mode", "", "runtime session mode to start in, for runtimes that offer modes")
	fs.StringVar(&f.profile, "profile", "", "policy profile: observe, standard, controlled or custom (default from config)")
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
	t := domain.Task{Workspace: ws, Prompt: domain.Text(prompt), Skills: f.skills, Provider: f.provider, Timeout: f.timeout, Profile: f.profile, Sandbox: f.sandbox}
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
	// chat keeps a conversation readable: one header instead of a session
	// line, and no per-turn summary unless verbose.
	chat, verbose bool
	mu            sync.Mutex
	midMsg        bool
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
		scripts := "scripts allowed"
		if !d.Scripts {
			scripts = "scripts withheld"
		}
		p.line("[skill] %s delivered (%s, %s)", d.SkillID, d.Mode, scripts)
	case domain.EventSessionStarted:
		var d domain.SessionStartedData
		json.Unmarshal(e.Data, &d)
		model := ""
		if d.Model != "" {
			model = ", model " + d.Model
		}
		if p.chat && !p.verbose {
			return
		}
		p.line("[session] %s on %s%s", e.SessionID, e.Provider, model)
	}
}

// endLine finishes a message that was being streamed.
func (p *printer) endLine() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.midMsg {
		fmt.Fprintln(p.env.Stdout)
		p.midMsg = false
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
	if p.chat && !p.verbose {
		return
	}
	fmt.Fprintf(p.env.Stderr, "[done] %s, stop %s%s\n", r.SessionID, r.StopReason, usage)
}

// promptMu keeps approval and input questions from interleaving on the terminal.
var promptMu sync.Mutex

// readLine reads one answer, or returns false when ctx ends first.
func readLine(ctx context.Context, in *bufio.Reader) (string, bool) {
	answer := make(chan string, 1)
	go func() {
		s, _ := in.ReadString('\n')
		answer <- strings.TrimSpace(s)
	}()
	select {
	case a := <-answer:
		return a, true
	case <-ctx.Done():
		return "", false
	}
}

// newElicitor asks the user on the terminal when the runtime asks a
// question. Without a terminal, questions are declined.
func newElicitor(env Env, in *bufio.Reader) domain.Elicitor {
	if !env.StdinTTY {
		return nil
	}
	return domain.ElicitorFunc(func(ctx context.Context, r domain.InputRequest) domain.InputResponse {
		promptMu.Lock()
		defer promptMu.Unlock()
		fmt.Fprintf(env.Stderr, "\n[input] %s\n", r.Message)
		if r.Mode == "url" {
			fmt.Fprintf(env.Stderr, "        open %s yourself, then press enter (n to decline): ", r.URL)
			a, ok := readLine(ctx, in)
			if !ok {
				return domain.InputResponse{Action: "cancel"}
			}
			if strings.EqualFold(a, "n") {
				return domain.InputResponse{Action: "decline"}
			}
			return domain.InputResponse{Action: "accept"}
		}
		var schema struct {
			Properties map[string]struct {
				Type  string   `json:"type"`
				Title string   `json:"title"`
				Enum  []string `json:"enum"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(r.Schema, &schema)
		names := make([]string, 0, len(schema.Properties))
		for n := range schema.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintln(env.Stderr, "        (an empty answer skips a field; /decline declines)")
		content := map[string]any{}
		for _, n := range names {
			f := schema.Properties[n]
			label := n
			if f.Title != "" {
				label = f.Title
			}
			if len(f.Enum) > 0 {
				label += " [" + strings.Join(f.Enum, "/") + "]"
			}
			if slices.Contains(schema.Required, n) {
				label += " (required)"
			}
			fmt.Fprintf(env.Stderr, "        %s: ", label)
			a, ok := readLine(ctx, in)
			switch {
			case !ok:
				return domain.InputResponse{Action: "cancel"}
			case a == "/decline":
				return domain.InputResponse{Action: "decline"}
			case a == "":
				continue
			}
			switch f.Type {
			case "boolean":
				content[n] = a == "y" || a == "yes" || a == "true"
			case "number", "integer":
				var x float64
				if _, err := fmt.Sscan(a, &x); err != nil {
					fmt.Fprintf(env.Stderr, "        %s is not a number; field skipped\n", a)
					continue
				}
				content[n] = x
			default:
				content[n] = a
			}
		}
		return domain.InputResponse{Action: "accept", Content: content}
	})
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
		return domain.ApproverFunc(func(ctx context.Context, r domain.ApprovalRequest) domain.ApprovalDecision {
			promptMu.Lock()
			defer promptMu.Unlock()
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
					d := r.AllowOnce("user")
					d.Remember = true
					return d
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
	res, err := svc.Run(ctx, t, service.RunOptions{Sink: pr, Approver: approver, Model: tf.model, Mode: tf.mode, Elicitor: newElicitor(env, in), Client: "cli"})
	if err != nil {
		pr.endLine()
		if domain.CodeOf(err) == domain.ErrAuthenticationRequired && !tf.asJSON {
			fmt.Fprintln(env.Stderr, "[hint] sign in to that runtime again, for example by starting it in a terminal and using its login command")
		}
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
	verbose := fs.Bool("v", false, "show the session id and a summary after each answer")
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
	pr := &printer{env: env, asJSON: tf.asJSON, quiet: tf.quiet, chat: true, verbose: *verbose}
	sess, err := svc.OpenSession(ctx, t, service.RunOptions{Sink: pr, Approver: approver, Model: tf.model, Mode: tf.mode, Elicitor: newElicitor(env, in), Client: "cli"})
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())
	return chatLoop(ctx, sess, strings.Join(pos, " "), in, pr, env)
}

func chatLoop(ctx context.Context, sess *service.Session, first string, in *bufio.Reader, pr *printer, env Env) error {
	if !pr.asJSON {
		if pr.verbose {
			fmt.Fprintf(env.Stderr, "[chat] session %s; type /exit to leave\n", sess.Record().ID)
		} else {
			fmt.Fprintf(env.Stderr, "chatting with %s; /exit to leave, /help for commands\n", sess.Record().ProviderID)
		}
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
		if strings.HasPrefix(next, "/") {
			chatCommand(ctx, sess, next, env)
			next = ""
			continue
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

// chatCommand handles the chat's slash commands.
func chatCommand(ctx context.Context, sess *service.Session, line string, env Env) {
	fields := strings.Fields(line)
	out := env.Stderr
	switch fields[0] {
	case "/help":
		fmt.Fprintln(out, "/modes, /mode <id>, /options, /set <option> <value>, /exit")
	case "/modes":
		cur, modes, err := sess.Modes()
		if err != nil {
			fmt.Fprintln(out, "rbridge:", err)
			return
		}
		if len(modes) == 0 {
			fmt.Fprintln(out, "this runtime offers no modes")
		}
		for _, m := range modes {
			mark := " "
			if m.ID == cur {
				mark = "*"
			}
			fmt.Fprintf(out, "%s %-12s %s %s\n", mark, m.ID, m.Name, m.Description)
		}
	case "/mode":
		if len(fields) != 2 {
			fmt.Fprintln(out, "usage: /mode <id>")
			return
		}
		if err := sess.SetMode(ctx, fields[1]); err != nil {
			fmt.Fprintln(out, "rbridge:", err)
			return
		}
		fmt.Fprintln(out, "[mode]", fields[1])
	case "/options":
		opts, err := sess.ConfigOptions()
		if err != nil {
			fmt.Fprintln(out, "rbridge:", err)
			return
		}
		if len(opts) == 0 {
			fmt.Fprintln(out, "this runtime offers no options")
		}
		for _, o := range opts {
			var values []string
			for _, v := range o.Options {
				values = append(values, v.Value)
			}
			fmt.Fprintf(out, "%-12s %-10s %s", o.ID, o.Current, o.Name)
			if len(values) > 0 {
				fmt.Fprintf(out, " (%s)", strings.Join(values, ", "))
			}
			fmt.Fprintln(out)
		}
	case "/set":
		if len(fields) != 3 {
			fmt.Fprintln(out, "usage: /set <option> <value>")
			return
		}
		if err := sess.SetConfigOption(ctx, fields[1], fields[2]); err != nil {
			fmt.Fprintln(out, "rbridge:", err)
			return
		}
		fmt.Fprintf(out, "[set] %s = %s\n", fields[1], fields[2])
	default:
		fmt.Fprintf(out, "unknown command %s; /help lists them\n", fields[0])
	}
}
