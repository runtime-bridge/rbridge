package service

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/policy"
)

func newBenchSequencer() *sequencer {
	return &sequencer{session: "ses_bench", provider: "bench", out: domain.EventSinkFunc(func(domain.Event) {}), now: time.Now}
}

var benchEvent = domain.NewEvent(domain.EventMessageDelta, domain.TextData{Text: "some streamed text from the runtime"})

// BenchmarkSequencer is the per-event cost rbridge adds between a runtime and the caller.
func BenchmarkSequencer(b *testing.B) {
	q := newBenchSequencer()
	b.ReportAllocs()
	for b.Loop() {
		q.Emit(benchEvent)
	}
}

func BenchmarkPolicyDecide(b *testing.B) {
	p, _ := policy.Builtin("standard")
	in, _ := json.Marshal(map[string]string{"file_path": "/work/proj/main.go"})
	req := domain.ApprovalRequest{Kind: "edit", Input: in, Options: []domain.ApprovalOption{{ID: "a", Kind: "allow_once"}}}
	ctx := policy.Context{Workspace: "/work/proj", Skills: map[string]domain.TrustState{"/s/x": domain.TrustTrusted}}
	b.ReportAllocs()
	for b.Loop() {
		p.Decide(req, ctx)
	}
}

// TestAllocationBudget keeps the hot paths from growing allocations unnoticed.
// The budgets have headroom over today's numbers; raise them only on purpose.
func TestAllocationBudget(t *testing.T) {
	q := newBenchSequencer()
	if n := testing.AllocsPerRun(1000, func() { q.Emit(benchEvent) }); n > 2 {
		t.Errorf("sequencer.Emit allocates %.0f times per event, budget 2", n)
	}
	p, _ := policy.Builtin("standard")
	in, _ := json.Marshal(map[string]string{"file_path": "/work/proj/main.go"})
	req := domain.ApprovalRequest{Kind: "edit", Input: in}
	ctx := policy.Context{Workspace: "/work/proj"}
	if n := testing.AllocsPerRun(1000, func() { p.Decide(req, ctx) }); n > 25 {
		t.Errorf("policy decision allocates %.0f times, budget 25", n)
	}
}
