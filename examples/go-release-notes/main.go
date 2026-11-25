// Command release-notes writes release notes for a Git repository. It does
// not contain an agent: it hands the commit history and a release-notes
// skill to an agent runtime already installed on the machine, through the
// rbridge Go library, and writes the answer to a file.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/runtime-bridge/rbridge/pkg/bridge"
)

//go:embed skills
var skills embed.FS

type config struct {
	Repo     string
	Since    string
	Version  string
	Out      string
	Provider string
	Model    string
	Verbose  bool
	Bridge   bridge.Options
}

func main() {
	var c config
	flag.StringVar(&c.Repo, "repo", ".", "Git repository")
	flag.StringVar(&c.Since, "since", "", "start of the range (default: the latest tag, or the whole history)")
	flag.StringVar(&c.Version, "version", "Unreleased", "heading for the notes")
	flag.StringVar(&c.Out, "out", "RELEASE_NOTES.md", "file to write")
	flag.StringVar(&c.Provider, "provider", "", "rbridge provider (default: the first available)")
	flag.StringVar(&c.Model, "model", "", "model for the runtime")
	flag.BoolVar(&c.Verbose, "v", false, "show the runtime's answer as it streams")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, c); err != nil {
		fmt.Fprintln(os.Stderr, "release-notes:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c config) error {
	repo, err := filepath.Abs(c.Repo)
	if err != nil {
		return err
	}
	commits, since, err := history(ctx, repo, c.Since)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return errors.New("no commits since " + since)
	}

	b, err := bridge.New(ctx, c.Bridge)
	if err != nil {
		return err
	}
	defer b.Close()
	if err := installSkill(ctx, b); err != nil {
		return err
	}

	prompt := "Write release notes for these commits (" + since + " to HEAD):\n\n" + strings.Join(commits, "\n\n")
	opts := []bridge.RunOption{}
	if c.Model != "" {
		opts = append(opts, bridge.WithModel(c.Model))
	}
	if c.Verbose {
		opts = append(opts, bridge.OnEvent(func(e bridge.Event) {
			if e.Type == bridge.EventMessageDelta {
				var d bridge.TextData
				if json.Unmarshal(e.Data, &d) == nil {
					fmt.Fprint(os.Stderr, d.Text)
				}
			}
		}))
	}
	res, err := b.Run(ctx, bridge.Task{
		Workspace: repo,
		Prompt:    bridge.Text(prompt),
		Skills:    []string{"release-notes"},
		Provider:  c.Provider,
		// The runtime only reads; this program writes the file.
		Profile: "observe",
	}, opts...)
	if err != nil {
		return err
	}
	notes := "## " + c.Version + "\n\n" + strings.TrimSpace(res.Output) + "\n"
	out := c.Out
	if !filepath.IsAbs(out) {
		out = filepath.Join(repo, out)
	}
	if err := os.WriteFile(out, []byte(notes), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s from %d commits using %s\n", out, len(commits), res.Provider)
	return nil
}

// history lists each commit after since (default: the latest tag) as
// "<hash> <subject>" followed by its message body, indented.
func history(ctx context.Context, repo, since string) ([]string, string, error) {
	git := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...).Output()
		return strings.TrimSpace(string(out)), err
	}
	if since == "" {
		if tag, err := git("describe", "--tags", "--abbrev=0"); err == nil && tag != "" {
			since = tag
		}
	}
	rng, label := "HEAD", "the first commit"
	if since != "" {
		rng, label = since+"..HEAD", since
	}
	log, err := git("log", "--no-merges", "--format=%x1e%h %s%n%w(0,4,4)%b", rng)
	if err != nil {
		return nil, "", fmt.Errorf("reading the history of %s: %w", repo, err)
	}
	var commits []string
	for c := range strings.SplitSeq(log, "\x1e") {
		if c = strings.TrimSpace(c); c != "" {
			commits = append(commits, c)
		}
	}
	return commits, label, nil
}

// installSkill adds the bundled skill to the library. This program wrote
// the skill, so it marks it reviewed; skills from elsewhere should be read
// before they are trusted.
func installSkill(ctx context.Context, b *bridge.Bridge) error {
	dir, err := os.MkdirTemp("", "release-notes-skill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	sub, _ := fs.Sub(skills, "skills/release-notes")
	if err := os.CopyFS(filepath.Join(dir, "release-notes"), sub); err != nil {
		return err
	}
	if _, err := b.AddSkill(ctx, filepath.Join(dir, "release-notes"), false); err != nil {
		return err
	}
	_, err = b.SetSkillTrust(ctx, "release-notes", bridge.TrustReviewed)
	return err
}
