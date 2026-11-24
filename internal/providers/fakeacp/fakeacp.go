// Package fakeacp is a scriptable ACP agent used by tests. Tests re-run their
// own binary with RBRIDGE_FAKE_ACP set and call Main from TestMain.
package fakeacp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// EnvScenario selects the behaviour; EnvEnable marks the re-exec.
const (
	EnvEnable   = "RBRIDGE_FAKE_ACP"
	EnvScenario = "RBRIDGE_FAKE_ACP_SCENARIO"
)

// Scenarios:
//
//	echo        streams "echo: <prompt>" in two chunks with a plan and a tool call
//	permission  asks to edit notes.txt; writes it through fs/write_text_file if allowed
//	readfile    reads the file named in the prompt through fs/read_text_file
//	slow        streams a chunk every 20ms until cancelled
//	hang        never answers a prompt and ignores cancel
//	auth        rejects session/new with "authentication required"
//	crash       exits in the middle of a prompt
//	spawn       starts a long-lived child and writes its pid to child.pid
//	env         replies with NAME=value for each variable named in the prompt
type agent struct {
	conn     *sdk.AgentSideConnection
	scenario string
	cwd      string

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	n       int
}

// Main runs the fake agent on stdin and stdout until stdin closes.
// With --version it prints a version, so detection can find it.
func Main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("fake-acp 1.19.6")
		return
	}
	a := &agent{scenario: os.Getenv(EnvScenario), cancels: map[string]context.CancelFunc{}}
	a.conn = sdk.NewAgentSideConnection(a, os.Stdout, os.Stdin)
	<-a.conn.Done()
}

func (a *agent) Initialize(context.Context, sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		AgentCapabilities: sdk.AgentCapabilities{
			LoadSession:         true,
			PromptCapabilities:  sdk.PromptCapabilities{Image: true},
			SessionCapabilities: sdk.SessionCapabilities{Resume: &sdk.SessionResumeCapabilities{}, Close: &sdk.SessionCloseCapabilities{}},
		},
		AgentInfo: &sdk.Implementation{Name: "fake-acp", Version: "0.0.1"},
	}, nil
}

func (a *agent) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}

func (a *agent) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}

func (a *agent) NewSession(_ context.Context, p sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if a.scenario == "auth" {
		return sdk.NewSessionResponse{}, sdk.NewAuthRequired(nil)
	}
	a.mu.Lock()
	a.n++
	id := fmt.Sprintf("fake-session-%d", a.n)
	a.cwd = p.Cwd
	a.mu.Unlock()
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id)}, nil
}

func (a *agent) ResumeSession(_ context.Context, p sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	a.mu.Lock()
	a.cwd = p.Cwd
	a.mu.Unlock()
	return sdk.ResumeSessionResponse{}, nil
}

func (a *agent) LoadSession(_ context.Context, p sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	return sdk.LoadSessionResponse{}, nil
}

func (a *agent) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}

func (a *agent) DeleteSession(context.Context, sdk.DeleteSessionRequest) (sdk.DeleteSessionResponse, error) {
	return sdk.DeleteSessionResponse{}, nil
}

func (a *agent) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}

func (a *agent) SetSessionConfigOption(_ context.Context, p sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	if p.ValueId != nil && strings.HasPrefix(string(p.ValueId.Value), "bad") {
		return sdk.SetSessionConfigOptionResponse{}, sdk.NewInvalidParams(map[string]string{"error": "unknown model"})
	}
	return sdk.SetSessionConfigOptionResponse{}, nil
}

func (a *agent) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}

func (a *agent) Cancel(_ context.Context, p sdk.CancelNotification) error {
	if a.scenario == "hang" {
		return nil
	}
	a.mu.Lock()
	if c := a.cancels[string(p.SessionId)]; c != nil {
		c()
	}
	a.mu.Unlock()
	return nil
}

func (a *agent) send(ctx context.Context, sid sdk.SessionId, u sdk.SessionUpdate) {
	_ = a.conn.SessionUpdate(ctx, sdk.SessionNotification{SessionId: sid, Update: u})
}

func promptText(p sdk.PromptRequest) string {
	var b strings.Builder
	for _, c := range p.Prompt {
		if c.Text != nil {
			b.WriteString(c.Text.Text)
		}
	}
	return b.String()
}

func (a *agent) Prompt(ctx context.Context, p sdk.PromptRequest) (sdk.PromptResponse, error) {
	sid := p.SessionId
	tctx, cancel := context.WithCancel(ctx)
	a.mu.Lock()
	a.cancels[string(sid)] = cancel
	cwd := a.cwd
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.cancels, string(sid))
		a.mu.Unlock()
		cancel()
	}()
	text := promptText(p)

	switch a.scenario {
	case "", "echo":
		a.send(ctx, sid, sdk.UpdatePlan(sdk.PlanEntry{Content: "echo the prompt", Priority: "medium", Status: "in_progress"}))
		a.send(ctx, sid, sdk.StartToolCall("tool-1", "Think about it", sdk.WithStartKind(sdk.ToolKindRead), sdk.WithStartStatus(sdk.ToolCallStatusInProgress)))
		a.send(ctx, sid, sdk.UpdateToolCall("tool-1", sdk.WithUpdateStatus(sdk.ToolCallStatusCompleted)))
		a.send(ctx, sid, sdk.UpdateAgentMessageText("echo: "))
		a.send(ctx, sid, sdk.UpdateAgentMessageText(text))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn, Usage: &sdk.Usage{InputTokens: 3, OutputTokens: 5, TotalTokens: 8}}, nil

	case "permission":
		target := filepath.Join(cwd, "notes.txt")
		r, err := a.conn.RequestPermission(ctx, sdk.RequestPermissionRequest{
			SessionId: sid,
			ToolCall:  sdk.ToolCallUpdate{ToolCallId: "edit-1", Title: sdk.PatchSet("Write notes.txt"), Kind: sdk.PatchSet(sdk.ToolKindEdit)},
			Options: []sdk.PermissionOption{
				{OptionId: "yes", Kind: sdk.PermissionOptionKindAllowOnce, Name: "Allow"},
				{OptionId: "no", Kind: sdk.PermissionOptionKindRejectOnce, Name: "Reject"},
			},
		})
		if err != nil {
			return sdk.PromptResponse{}, err
		}
		if r.Outcome.Selected != nil && r.Outcome.Selected.OptionId == "yes" {
			if _, err := a.conn.WriteTextFile(ctx, sdk.WriteTextFileRequest{SessionId: sid, Path: target, Content: "notes from the fake agent\n"}); err != nil {
				a.send(ctx, sid, sdk.UpdateAgentMessageText("write failed: "+err.Error()))
			} else {
				a.send(ctx, sid, sdk.UpdateAgentMessageText("allowed"))
			}
		} else {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("denied"))
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "readfile":
		r, err := a.conn.ReadTextFile(ctx, sdk.ReadTextFileRequest{SessionId: sid, Path: strings.TrimSpace(text)})
		if err != nil {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("error: "+err.Error()))
		} else {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("content: "+r.Content))
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "slow":
		for i := 0; ; i++ {
			select {
			case <-tctx.Done():
				return sdk.PromptResponse{StopReason: sdk.StopReasonCancelled}, nil
			case <-time.After(20 * time.Millisecond):
				a.send(ctx, sid, sdk.UpdateAgentMessageText(strconv.Itoa(i)+" "))
			}
		}

	case "hang":
		select {}

	case "crash":
		a.send(ctx, sid, sdk.UpdateAgentMessageText("about to crash"))
		time.Sleep(50 * time.Millisecond)
		fmt.Fprintln(os.Stderr, "fake agent: simulated crash")
		os.Exit(3)

	case "env":
		var parts []string
		for _, name := range strings.Fields(text) {
			parts = append(parts, name+"="+os.Getenv(name))
		}
		a.send(ctx, sid, sdk.UpdateAgentMessageText(strings.Join(parts, " ")))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "spawn":
		child := exec.Command("sleep", "60")
		if err := child.Start(); err != nil {
			return sdk.PromptResponse{}, err
		}
		_ = os.WriteFile(filepath.Join(cwd, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		a.send(ctx, sid, sdk.UpdateAgentMessageText("spawned"))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
	}
	return sdk.PromptResponse{}, fmt.Errorf("unknown scenario %q", a.scenario)
}
