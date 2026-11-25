package suggest

import "testing"

func TestClosest(t *testing.T) {
	providers := []string{"claude-code", "claude-code-pro", "opencode", "ollama"}
	commands := []string{"list", "add", "show", "trust", "remove", "skills", "sessions"}
	for _, c := range []struct {
		in   string
		opts []string
		want string
	}{
		{"claude", providers, "claude-code"},
		{"opencod", providers, "opencode"},
		{"olama", providers, "ollama"},
		{"skill", commands, "skills"},
		{"sesions", commands, "sessions"},
		{"lsit", commands, "list"},
		{"zzz", commands, ""},
		{"x", commands, ""},
	} {
		if got := Closest(c.in, c.opts); got != c.want {
			t.Errorf("Closest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
