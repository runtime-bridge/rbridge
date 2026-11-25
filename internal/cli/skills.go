package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/skills/deliver"
	skillreg "github.com/runtime-bridge/rbridge/internal/skills/registry"
	"github.com/runtime-bridge/rbridge/internal/skills/spec"
)

func cmdSkills(ctx context.Context, args []string, env Env) error {
	if len(args) == 0 {
		args = []string{"list"}
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return skillsList(ctx, rest, env)
	case "add":
		return skillsAdd(ctx, rest, env)
	case "show":
		return skillsShow(ctx, rest, env)
	case "trust":
		return skillsTrust(ctx, rest, env)
	case "verify":
		return skillsVerify(ctx, rest, env)
	case "remove":
		return skillsRemove(ctx, rest, env)
	case "deliver":
		return skillsDeliver(ctx, rest, env)
	}
	return domain.Errorf(domain.ErrInvalidInput, "unknown skills command %q (list, add, show, trust, verify, remove, deliver)", sub)
}

func skillsList(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills list", env)
	asJSON := fs.Bool("json", false, "print JSON")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	list, err := svc.Skills().List(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		if list == nil {
			list = []domain.SkillRecord{}
		}
		return writeJSON(env.Stdout, list)
	}
	if len(list) == 0 {
		fmt.Fprintln(env.Stdout, "no skills yet; add one with: rbridge skills add <dir>")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SKILL\tVERSION\tTRUST\tSCRIPTS\tSOURCE\tDIGEST")
	for _, r := range list {
		scripts := "no"
		if r.HasScripts {
			scripts = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, dash(r.Version), r.Trust, scripts, sourceText(r.Source), shortDigest(r.Digest))
	}
	return tw.Flush()
}

func sourceText(s domain.SkillSource) string {
	switch s.Type {
	case "git":
		rev := s.Revision
		if len(rev) > 12 {
			rev = rev[:12]
		}
		out := s.URL + "@" + rev
		if s.Subdir != "" {
			out += "#" + s.Subdir
		}
		return out
	case "link":
		return "link:" + s.Path
	}
	return s.Path
}

func shortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func skillsAdd(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills add", env)
	rev := fs.String("rev", "", "Git revision (a full commit hash unless pinning is turned off)")
	subdir := fs.String("subdir", "", "skill directory inside the Git repository")
	link := fs.Bool("link", false, "use a local directory in place instead of copying it")
	trust := fs.String("trust", "", "set a trust state right after adding (for skills you wrote yourself)")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge skills add <dir | git-url --rev COMMIT [--subdir DIR]>")
	}
	src := pos[0]
	req := skillreg.AddRequest{Link: *link, Rev: *rev, Subdir: *subdir}
	if strings.Contains(src, "://") || strings.HasPrefix(src, "git@") {
		req.URL = src
	} else {
		if !filepath.IsAbs(src) {
			src = filepath.Join(env.Dir, src)
		}
		req.Path = src
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	reg := svc.Skills()
	res, err := reg.Add(ctx, req)
	if err != nil {
		return err
	}
	if *trust != "" {
		state, err := skillreg.ParseTrust(*trust)
		if err != nil {
			return err
		}
		if res.Record, err = reg.SetTrust(ctx, res.Record.ID, state); err != nil {
			return err
		}
	}
	if *asJSON {
		return writeJSON(env.Stdout, res)
	}
	switch {
	case res.Unchanged:
		fmt.Fprintf(env.Stdout, "%s is already installed with the same content (trust %s)\n", res.Record.ID, res.Record.Trust)
	case res.Replaced != "":
		fmt.Fprintf(env.Stdout, "updated %s (%s, was %s); trust reset to %s\n", res.Record.ID, shortDigest(res.Record.Digest), shortDigest(res.Replaced), res.Record.Trust)
	default:
		fmt.Fprintf(env.Stdout, "added %s (%s), trust %s\n", res.Record.ID, shortDigest(res.Record.Digest), res.Record.Trust)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(env.Stderr, "[warn] %s\n", w)
	}
	if res.Record.Trust == domain.TrustUnreviewed {
		fmt.Fprintf(env.Stdout, "review it with: rbridge skills show %s, then: rbridge skills trust %s reviewed\n", res.Record.ID, res.Record.ID)
	}
	return nil
}

func skillsShow(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills show", env)
	asJSON := fs.Bool("json", false, "print JSON")
	full := fs.Bool("full", false, "print the whole SKILL.md")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge skills show <id>")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	reg := svc.Skills()
	rec, err := reg.Require(ctx, pos[0])
	if err != nil {
		return err
	}
	dir := reg.Path(rec)
	sk, parseErr := spec.Parse(dir, reg.Limits)
	verifyErr := reg.Verify(ctx, rec)
	if *asJSON {
		out := map[string]any{"record": rec, "path": dir, "files": sk.Files, "scripts": sk.Scripts, "warnings": sk.Warnings, "verified": verifyErr == nil}
		if verifyErr != nil {
			out["verify_error"] = verifyErr.Error()
		}
		return writeJSON(env.Stdout, out)
	}
	w := env.Stdout
	fmt.Fprintf(w, "%s\n  %s\n\n", rec.ID, rec.Description)
	fmt.Fprintf(w, "  version:   %s\n  trust:     %s\n  license:   %s\n  source:    %s\n  digest:    %s\n  path:      %s\n", dash(rec.Version), rec.Trust, dash(rec.License), sourceText(rec.Source), rec.Digest, dir)
	if verifyErr != nil {
		fmt.Fprintf(w, "  verify:    FAILED: %v\n", verifyErr)
	} else {
		fmt.Fprintln(w, "  verify:    ok")
	}
	if parseErr == nil {
		fmt.Fprintf(w, "\nFiles (%d):\n", len(sk.Files))
		for _, f := range sk.Files {
			mark := ""
			for _, s := range sk.Scripts {
				if s == f {
					mark = "  (script)"
				}
			}
			fmt.Fprintf(w, "  %s%s\n", f, mark)
		}
		printList(w, "Warnings", sk.Warnings)
		body := sk.Body
		if !*full && len(body) > 2000 {
			body = body[:2000] + "\n... (use --full to see everything)"
		}
		fmt.Fprintf(w, "\nSKILL.md:\n%s\n", body)
	}
	if len(sk.Scripts) > 0 {
		fmt.Fprintln(w, "\nScripts are delivered only when trust is reviewed or trusted.")
	}
	return nil
}

func skillsTrust(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills trust", env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 2 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge skills trust <id> unreviewed|reviewed|trusted|restricted|blocked")
	}
	state, err := skillreg.ParseTrust(pos[1])
	if err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	rec, err := svc.Skills().SetTrust(ctx, pos[0], state)
	if err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "%s is now %s\n", rec.ID, rec.Trust)
	return nil
}

func skillsVerify(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills verify", env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	reg := svc.Skills()
	var recs []domain.SkillRecord
	if len(pos) == 0 {
		if recs, err = reg.List(ctx); err != nil {
			return err
		}
	}
	for _, id := range pos {
		rec, err := reg.Require(ctx, id)
		if err != nil {
			return err
		}
		recs = append(recs, rec)
	}
	failed := 0
	for _, r := range recs {
		if err := reg.Verify(ctx, r); err != nil {
			failed++
			fmt.Fprintf(env.Stdout, "FAIL %s: %v\n", r.ID, err)
		} else {
			fmt.Fprintf(env.Stdout, "ok   %s %s\n", r.ID, shortDigest(r.Digest))
		}
	}
	if failed > 0 {
		return errSilent{code: domain.ErrSkillDigestMismatch.ExitCode()}
	}
	return nil
}

func skillsRemove(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills remove", env)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge skills remove <id>")
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	if err := svc.Skills().Remove(ctx, pos[0]); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "removed %s\n", pos[0])
	return nil
}

// skillsDeliver lays skills out for a provider in a directory of the user's
// choosing, for use with a runtime started by hand.
func skillsDeliver(ctx context.Context, args []string, env Env) error {
	fs := newFlags("skills deliver", env)
	provider := fs.String("provider", "", "provider to lay the skills out for")
	to := fs.String("to", "", "empty or new directory to write into")
	workspace := fs.String("workspace", "", "project to check for name clashes (default: current directory)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 || *provider == "" || *to == "" {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge skills deliver <id>... --provider ID --to DIR")
	}
	out, err := filepath.Abs(*to)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		return domain.Errorf(domain.ErrInvalidInput, "%s is not empty", out)
	}
	ws := *workspace
	if ws == "" {
		ws = env.Dir
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	st, err := svc.Provider(ctx, *provider, false)
	if err != nil {
		return err
	}
	reg := svc.Skills()
	var items []deliver.Item
	for _, id := range pos {
		rec, err := reg.Require(ctx, id)
		if err != nil {
			return err
		}
		if rec.Trust == domain.TrustBlocked {
			return domain.Errorf(domain.ErrSkillBlocked, "skill %s is blocked", id)
		}
		if err := reg.Verify(ctx, rec); err != nil {
			return err
		}
		sk, err := spec.Parse(reg.Path(rec), reg.Limits)
		if err != nil {
			return err
		}
		items = append(items, deliver.Item{Record: rec, Dir: reg.Path(rec), Scripts: rec.Trust.ScriptsAllowed(), ScriptFiles: sk.Scripts})
	}
	strategy, layout := svc.DeliveryFor(*provider)
	plan, err := deliver.Prepare(deliver.Request{Strategy: deliver.Strategy(strategy), Layout: layout, Scratch: out, Items: items, Getenv: env.Getenv})
	if err != nil {
		return err
	}
	for _, d := range plan.Deliveries {
		fmt.Fprintf(env.Stdout, "%s: %s\n", d.SkillID, d.Mode)
	}
	if len(plan.SkillDirs) > 0 {
		fmt.Fprintf(env.Stdout, "\nstart %s with: --add-dir %s\n", st.DisplayName, strings.Join(plan.SkillDirs, " "))
	}
	for k, v := range plan.Env {
		fmt.Fprintf(env.Stdout, "\nset %s=%s before starting %s\n", k, v, st.DisplayName)
	}
	for _, c := range deliver.FindClashes(pos, ws, st.Skills.Project, st.Skills.User) {
		fmt.Fprintf(env.Stderr, "[warn] another skill named %s exists at %s\n", c.SkillID, c.Path)
	}
	return nil
}
