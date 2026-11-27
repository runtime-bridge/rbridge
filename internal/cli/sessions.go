package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/runtime-bridge/rbridge/internal/daemon"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
	"github.com/runtime-bridge/rbridge/pkg/bridge"
)

func cmdSessions(ctx context.Context, args []string, env Env) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	sessionCommands := []string{"list", "show", "resume", "cancel", "prune"}
	switch sub = subcommand(sub, sessionCommands); sub {
	case "list":
		return sessionsList(ctx, args, env)
	case "show":
		return sessionsShow(ctx, args, env)
	case "resume":
		return sessionsResume(ctx, args, env)
	case "cancel":
		return sessionsCancel(ctx, args, env)
	case "prune":
		return sessionsPrune(ctx, args, env)
	}
	return unknownSub("sessions", sub, sessionCommands)
}

func sessionsList(ctx context.Context, args []string, env Env) error {
	fs := newFlags("sessions list", env)
	asJSON := fs.Bool("json", false, "print JSON")
	limit := fs.Int("limit", 20, "show at most this many sessions (0 for all)")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	list, err := svc.Sessions(ctx)
	if err != nil {
		return err
	}
	if *limit > 0 && len(list) > *limit {
		list = list[:*limit]
	}
	if *asJSON {
		return writeJSON(env.Stdout, list)
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tPROVIDER\tSTATE\tTURNS\tLAST\tUPDATED\tWORKSPACE")
	for _, r := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", r.ID, r.ProviderID, r.State, r.Turns, dash(string(r.LastStop)), r.UpdatedAt.Local().Format("2006-01-02 15:04"), r.Workspace)
	}
	return tw.Flush()
}

func sessionsShow(ctx context.Context, args []string, env Env) error {
	fs := newFlags("sessions show", env)
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge sessions show <id>")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	r, err := svc.SessionRecord(ctx, pos[0])
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(env.Stdout, r)
	}
	w := env.Stdout
	fmt.Fprintf(w, "session:    %s\nprovider:   %s\nnative id:  %s\nstate:      %s\nturns:      %d\nworkspace:  %s\n", r.ID, r.ProviderID, dash(r.NativeID), r.State, r.Turns, r.Workspace)
	if r.Model != "" {
		fmt.Fprintf(w, "model:      %s\n", r.Model)
	}
	fmt.Fprintf(w, "created:    %s\nupdated:    %s\n", r.CreatedAt.Local().Format("2006-01-02 15:04:05"), r.UpdatedAt.Local().Format("2006-01-02 15:04:05"))
	if r.LastStop != "" {
		fmt.Fprintf(w, "last stop:  %s\n", r.LastStop)
	}
	if r.LastError != "" {
		fmt.Fprintf(w, "last error: %s\n", r.LastError)
	}
	for _, s := range r.Skills {
		fmt.Fprintf(w, "skill:      %s (%s, %s)\n", s.SkillID, s.Mode, s.Digest)
	}
	logPath := filepath.Join(svc.Paths().DataDir, "events", r.ID+".jsonl")
	if _, err := os.Stat(logPath); err == nil {
		fmt.Fprintf(w, "events:     %s\n", logPath)
	}
	return nil
}

func sessionsResume(ctx context.Context, args []string, env Env) error {
	fs := newFlags("sessions resume", env)
	var tf taskFlags
	tf.register(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge sessions resume <id> [prompt]")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	in := bufio.NewReader(env.Stdin)
	approver, err := newApprover(tf.approve, env, in)
	if err != nil {
		return err
	}
	pr := &printer{env: env, asJSON: tf.asJSON, quiet: tf.quiet}
	sess, err := svc.ResumeSession(ctx, pos[0], service.RunOptions{Sink: pr, Approver: approver, Model: tf.model, Profile: tf.profile, Client: "cli"})
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())
	prompt := strings.Join(pos[1:], " ")
	if prompt != "" {
		res, err := sess.Prompt(ctx, domain.Text(prompt))
		if err != nil {
			return err
		}
		pr.result(res)
		return nil
	}
	return chatLoop(ctx, sess, "", in, pr, env)
}

func sessionsCancel(ctx context.Context, args []string, env Env) error {
	fs := newFlags("sessions cancel", env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge sessions cancel <id>")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	// A session the daemon runs is closed by the daemon, on its own: the
	// daemon is shared, and interrupting it would end every session it has.
	if rec, err := svc.SessionRecord(ctx, pos[0]); err == nil {
		if st, err := daemon.ReadState(daemon.FilesFor(svc.Paths())); err == nil && st.PID != 0 && st.PID == rec.OwnerPID {
			r, err := bridge.Connect(ctx, bridge.ConnectOptions{Socket: st.Socket, Client: rec.Client})
			if err != nil {
				return err
			}
			defer r.Close()
			if err := r.CloseSession(ctx, rec.ID); err != nil {
				return err
			}
			fmt.Fprintf(env.Stdout, "closed %s in the rbridge daemon\n", rec.ID)
			return nil
		}
	}
	if err := svc.CancelSession(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "asked the rbridge process running %s to cancel it\n", pos[0])
	return nil
}

func sessionsPrune(ctx context.Context, args []string, env Env) error {
	fs := newFlags("sessions prune", env)
	older := fs.Duration("older-than", 30*24*time.Hour, "delete finished sessions last used before this long ago")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	n, err := svc.PruneSessions(ctx, *older)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "pruned %d sessions\n", n)
	return nil
}
