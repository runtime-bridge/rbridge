package cli

import (
	"context"
	"fmt"

	"go.yaml.in/yaml/v3"

	"github.com/rbridge/rbridge/internal/config"
)

func cmdConfig(_ context.Context, args []string, env Env) error {
	fs := newFlags("config", env)
	defaults := fs.Bool("defaults", false, "print the built-in defaults instead of the effective configuration")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg := config.Defaults()
	if !*defaults {
		svc, err := newService(env)
		if err != nil {
			return err
		}
		cfg = svc.Config()
		fmt.Fprintln(env.Stdout, "# layers, lowest first:")
		for _, l := range svc.Layers() {
			state := "not found"
			if l.Found {
				state = "loaded"
			}
			if l.Path == "" {
				state = "built in"
			}
			fmt.Fprintf(env.Stdout, "#   %-8s %s %s\n", l.Name, state, l.Path)
			for _, k := range l.Ignored {
				fmt.Fprintf(env.Stdout, "#            ignored locked key %s\n", k)
			}
		}
		fmt.Fprintf(env.Stdout, "# paths: config %s, data %s, cache %s, runtime %s\n", svc.Paths().ConfigDir, svc.Paths().DataDir, svc.Paths().CacheDir, svc.Paths().RuntimeDir)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = env.Stdout.Write(b)
	return err
}
