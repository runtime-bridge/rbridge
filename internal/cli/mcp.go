package cli

import (
	"context"

	"github.com/runtime-bridge/rbridge/internal/mcp"
)

func cmdMCP(ctx context.Context, args []string, env Env) error {
	fs := newFlags("mcp", env)
	profile := fs.String("profile", "observe", "profile for tasks that do not name one")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	svc, err := newService(env)
	if err != nil {
		return err
	}
	return mcp.Serve(ctx, env.Stdin, env.Stdout, svc, mcp.Options{Profile: *profile})
}
