package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/runtime-bridge/rbridge/internal/config"
	"github.com/runtime-bridge/rbridge/internal/domain"
	skillreg "github.com/runtime-bridge/rbridge/internal/skills/registry"
)

// starterConfig is written by rbridge init when no configuration is given.
// Every setting is commented out, so the built-in defaults apply until one
// is turned on.
const starterConfig = `# rbridge configuration. Every line below is a built-in default, commented
# out; remove the # to change one. rbridge config prints what is in effect,
# and docs/configuration.md describes every setting.
version: 1

# execution:
#   max_concurrent_sessions: 2      # sessions running at once
#   resource_ceiling_percent: 0     # e.g. 85: fewer sessions as the machine gets busy, never zero
#   slot_wait: 30s                  # how long a new session waits for room
#   task_timeout: 30m               # per turn
#   idle_timeout: 5m                # a turn that goes quiet is stopped

# policy:
#   default_profile: standard       # observe, standard, controlled or custom
#   approval_timeout: 2m

# sandbox:
#   mode: off                       # files or full, Linux only

# daemon:
#   max_sessions_per_client: 8      # open sessions per client name; 0 is no limit

# audit:
#   level: metadata                 # events or full keep more
`

type initResult struct {
	Home       string                  `json:"home"` // RBRIDGE_HOME, or the configuration folder
	ConfigFile string                  `json:"config_file"`
	Config     string                  `json:"config"` // written, starter, replaced, unchanged or kept
	Backup     string                  `json:"backup,omitempty"`
	Skills     *skillreg.AddManyResult `json:"skills,omitempty"`
}

func cmdInit(ctx context.Context, args []string, env Env) error {
	flags := newFlags("init", env)
	from := flags.String("config", "", "configuration file to install (default: a commented starter file)")
	force := flags.Bool("force", false, "replace a different configuration that is already there (a backup is kept)")
	var skillDirs, only, exclude listFlag
	flags.Var(&skillDirs, "skills", "folder of skills to add as well; repeatable")
	flags.Var(&only, "only", "with --skills: add only these names or patterns")
	flags.Var(&exclude, "exclude", "with --skills: leave out these names or patterns")
	trust := flags.String("trust", "", "with --skills: trust state for every skill added")
	asJSON := flags.Bool("json", false, "print JSON")
	pos, err := parse(flags, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return domain.Errorf(domain.ErrInvalidInput, "usage: rbridge init [--config FILE] [--force] [--skills DIR]... [--only NAMES] [--exclude NAMES] [--trust STATE]")
	}
	if len(skillDirs) == 0 && (len(only) > 0 || len(exclude) > 0 || *trust != "") {
		return domain.Errorf(domain.ErrInvalidInput, "--only, --exclude and --trust go with --skills")
	}
	var state domain.TrustState
	if *trust != "" {
		if state, err = skillreg.ParseTrust(*trust); err != nil {
			return err
		}
	}

	paths, err := config.ResolvePaths(config.Env{GOOS: env.GOOS, Getenv: env.Getenv, HomeDir: env.Home, UID: env.UID})
	if err != nil {
		return domain.Errorf(domain.ErrInvalidInput, "%v", err)
	}
	for _, d := range []string{paths.ConfigDir, paths.DataDir, paths.CacheDir, paths.RuntimeDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	res := initResult{Home: env.Getenv("RBRIDGE_HOME"), ConfigFile: paths.UserConfigFile()}
	if res.Home == "" {
		res.Home = paths.ConfigDir
	}

	want := []byte(starterConfig)
	if *from != "" {
		src := *from
		if !filepath.IsAbs(src) {
			src = filepath.Join(env.Dir, src)
		}
		if want, err = os.ReadFile(src); err != nil {
			return domain.Errorf(domain.ErrInvalidInput, "read %s: %v", *from, err)
		}
		if err := checkConfig(paths, want); err != nil {
			return domain.Errorf(domain.ErrInvalidInput, "%s is not a valid rbridge configuration: %v", *from, err)
		}
	}
	have, err := os.ReadFile(res.ConfigFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		res.Config = "written"
		if *from == "" {
			res.Config = "starter"
		}
	case err != nil:
		return err
	case bytes.Equal(have, want):
		res.Config = "unchanged"
	case *from == "":
		res.Config = "kept"
	case !*force:
		return domain.Errorf(domain.ErrInvalidInput, "%s already holds a different configuration; add --force to replace it (the old file is kept as config.yaml.bak)", res.ConfigFile)
	default:
		res.Backup = res.ConfigFile + ".bak"
		if err := os.WriteFile(res.Backup, have, 0o600); err != nil {
			return err
		}
		res.Config = "replaced"
	}
	if res.Config == "written" || res.Config == "starter" || res.Config == "replaced" {
		if err := os.WriteFile(res.ConfigFile, want, 0o600); err != nil {
			return err
		}
	}

	if len(skillDirs) > 0 {
		svc, err := newService(env)
		if err != nil {
			return err
		}
		req := skillreg.AddManyRequest{Only: only, Exclude: exclude}
		for _, d := range skillDirs {
			if !filepath.IsAbs(d) {
				d = filepath.Join(env.Dir, d)
			}
			req.Paths = append(req.Paths, d)
		}
		if state != "" {
			req.SetTrust = func(ctx context.Context, id string) (domain.SkillRecord, error) {
				return svc.SetSkillTrust(ctx, id, state)
			}
		}
		added, err := svc.Skills().AddMany(ctx, req)
		if err != nil {
			return err
		}
		res.Skills = &added
	}

	if *asJSON {
		if err := writeJSON(env.Stdout, res); err != nil {
			return err
		}
	} else {
		if env.Getenv("RBRIDGE_HOME") != "" {
			fmt.Fprintf(env.Stdout, "rbridge home: %s\n", res.Home)
		} else {
			fmt.Fprintf(env.Stdout, "rbridge folders: configuration %s, data %s\n", paths.ConfigDir, paths.DataDir)
		}
		switch res.Config {
		case "starter":
			fmt.Fprintf(env.Stdout, "configuration: wrote a starter file to %s (defaults until you change it)\n", res.ConfigFile)
		case "written":
			fmt.Fprintf(env.Stdout, "configuration: installed %s as %s\n", *from, res.ConfigFile)
		case "replaced":
			fmt.Fprintf(env.Stdout, "configuration: replaced %s with %s (old file kept as %s)\n", res.ConfigFile, *from, res.Backup)
		case "unchanged":
			fmt.Fprintf(env.Stdout, "configuration: %s is already up to date\n", res.ConfigFile)
		case "kept":
			fmt.Fprintf(env.Stdout, "configuration: kept the existing %s\n", res.ConfigFile)
		}
		if res.Skills != nil {
			var unreviewed []string
			for _, it := range res.Skills.Items {
				printAdded(env, it)
				if it.Result != nil && it.Result.Record.Trust == domain.TrustUnreviewed {
					unreviewed = append(unreviewed, it.ID)
				}
			}
			for _, w := range res.Skills.Warnings {
				fmt.Fprintf(env.Stderr, "[warn] %s\n", w)
			}
			fmt.Fprintln(env.Stdout, addSummary(*res.Skills))
			printReviewHint(env, unreviewed, domain.TrustUnreviewed)
		}
		fmt.Fprintln(env.Stdout, "next: rbridge doctor, then rbridge run \"...\" or rbridge daemon start")
	}
	if res.Skills != nil {
		if n := res.Skills.Failed(); n > 0 {
			return domain.Errorf(domain.ErrSkillInvalid, "%d of %d skills could not be added", n, len(res.Skills.Items))
		}
	}
	return nil
}

// checkConfig loads b as the user layer, on top of the defaults and any
// system configuration, without touching the real file.
func checkConfig(paths config.Paths, b []byte) error {
	dir, err := os.MkdirTemp("", "rbridge-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), b, 0o600); err != nil {
		return err
	}
	paths.ConfigDir = dir
	_, _, err = config.Load(paths, config.LoadOptions{})
	return err
}
