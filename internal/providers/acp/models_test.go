package acp

import (
	"strings"
	"testing"
)

func TestMatchModel(t *testing.T) {
	choices := []string{"opencode/big-pickle", "opencode/beam-free", "ollama/qwen3:8b", "other/beam-free"}
	if got, err := matchModel("big-pickle", choices); err != nil || got != "opencode/big-pickle" {
		t.Fatalf("short name: %q %v", got, err)
	}
	if got, err := matchModel("ollama/qwen3:8b", choices); err != nil || got != "ollama/qwen3:8b" {
		t.Fatalf("full name: %q %v", got, err)
	}
	if _, err := matchModel("beam-free", choices); err == nil || !strings.Contains(err.Error(), "could be") {
		t.Fatalf("ambiguous: %v", err)
	}
	if _, err := matchModel("big-pickel", choices); err == nil || !strings.Contains(err.Error(), "did you mean") {
		t.Fatalf("typo: %v", err)
	}
	many := make([]string, 15)
	for i := range many {
		many[i] = "p/m" + string(rune('a'+i))
	}
	if _, err := matchModel("zzz", many); err == nil || !strings.Contains(err.Error(), "and 5 more") {
		t.Fatalf("long list: %v", err)
	}
}
