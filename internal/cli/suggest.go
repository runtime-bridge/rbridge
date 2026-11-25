package cli

import (
	"strings"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/suggest"
)

var topCommands = []string{"help", "version", "providers", "doctor", "run", "chat", "sessions", "skills", "audit", "daemon", "mcp", "config", "init", "uninstall"}

// aliases are the names people type out of habit for a subcommand.
var aliases = map[string]string{"ls": "list", "rm": "remove", "delete": "remove", "del": "remove", "info": "show", "get": "show"}

// subcommand resolves an alias to the group's own name for it.
func subcommand(sub string, choices []string) string {
	if a, ok := aliases[sub]; ok {
		for _, c := range choices {
			if c == a {
				return a
			}
		}
	}
	return sub
}

func unknownSub(group, sub string, choices []string) error {
	return domain.Errorf(domain.ErrInvalidInput, "unknown %s command %q%s (%s)", group, sub, suggest.DidYouMean(sub, choices), strings.Join(choices, ", "))
}
