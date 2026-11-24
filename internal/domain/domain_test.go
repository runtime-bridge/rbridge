package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestCapabilityMergeRespectsSource(t *testing.T) {
	base := CapabilitySet{
		CapSkills:    {Supported: SupportYes, Source: SourceHandshake},
		CapStreaming: {Supported: SupportUnknown, Source: SourceDefault},
	}
	got := base.Merge(CapabilitySet{
		CapSkills:    {Supported: SupportNo, Source: SourceManifest},
		CapStreaming: {Supported: SupportYes, Source: SourceManifest},
		CapUsage:     {Supported: SupportYes, Source: SourceProbe},
	})
	if got[CapSkills].Supported != SupportYes {
		t.Error("a manifest value must not override a handshake value")
	}
	if got[CapStreaming].Supported != SupportYes || got[CapUsage].Supported != SupportYes {
		t.Error("higher or equal sources should apply")
	}
	if base[CapStreaming].Supported != SupportUnknown {
		t.Error("Merge must not modify its receiver")
	}
	if c := got.Get(CapOffline); c.Supported != SupportUnknown || c.Source != SourceDefault {
		t.Errorf("missing capability should be unknown, got %+v", c)
	}
}

func TestCapabilityNamesOrder(t *testing.T) {
	s := CapabilitySet{"zeta": {}, CapUsage: {}, CapStreaming: {}, "alpha": {}}
	got := fmt.Sprint(s.Names())
	if got != "[streaming usage alpha zeta]" {
		t.Fatalf("names = %s", got)
	}
}

func TestErrorCodes(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", Errorf(ErrProviderUnavailable, "claude-code is not runnable"))
	if CodeOf(err) != ErrProviderUnavailable {
		t.Fatalf("code = %s", CodeOf(err))
	}
	if CodeOf(errors.New("plain")) != ErrInternal {
		t.Fatal("plain errors are internal")
	}
	if ErrProviderUnavailable.ExitCode() != 11 || ErrInternal.ExitCode() != 70 || ErrInvalidInput.ExitCode() != 2 {
		t.Fatal("exit codes changed")
	}
}

func TestTaskValidate(t *testing.T) {
	ws := t.TempDir()
	ok := Task{Workspace: ws, Prompt: Text("hi")}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []Task{
		{Prompt: Text("hi")},
		{Workspace: "relative", Prompt: Text("hi")},
		{Workspace: ws},
		{Workspace: ws, Prompt: []ContentBlock{{}}},
		{Workspace: ws, Prompt: Text("hi"), Timeout: -1},
	}
	for i, b := range bad {
		if CodeOf(b.Validate()) != ErrInvalidInput {
			t.Errorf("case %d accepted", i)
		}
	}
}

func TestSessionTransitions(t *testing.T) {
	if !CanTransition(StateReady, StateRunning) || !CanTransition(StateRunning, StateAwaitingApproval) {
		t.Error("expected transitions missing")
	}
	if CanTransition(StateClosed, StateRunning) || CanTransition(StateCreated, StateRunning) {
		t.Error("invalid transition allowed")
	}
	if !StateClosed.Terminal() || StateInterrupted.Terminal() {
		t.Error("terminal states wrong")
	}
}

func TestTrust(t *testing.T) {
	if TrustUnreviewed.ScriptsAllowed() || TrustRestricted.ScriptsAllowed() || TrustBlocked.InstructionsUsable() {
		t.Error("trust rules wrong")
	}
	if !TrustTrusted.ScriptsAllowed() || !TrustRestricted.InstructionsUsable() {
		t.Error("trust rules wrong")
	}
}
