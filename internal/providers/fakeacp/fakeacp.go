// Package fakeacp is a scriptable ACP agent used by tests. Tests re-run their
// own binary with RBRIDGE_FAKE_ACP set and call Main from TestMain.
package fakeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
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
//	escape      writes files and opens connections itself ("write:a,b dial:host:port")
//	terminal    runs the prompt with sh -c in a client terminal and replies with its output
//	elicit      asks the user for a name through a form
//	(echo also answers "mode?" with the current mode and model)
//	writedirect writes direct.txt through fs/write_text_file without asking
type agent struct {
	conn     *sdk.AgentSideConnection
	scenario string
	cwd      string

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	n       int
	mode    string
	model   string
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
	version := sdk.ProtocolVersion(sdk.ProtocolVersionNumber)
	if v := os.Getenv("RBRIDGE_FAKE_ACP_PROTOCOL"); v != "" {
		n, _ := strconv.Atoi(v)
		version = sdk.ProtocolVersion(n)
	}
	return sdk.InitializeResponse{
		ProtocolVersion: version,
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
	a.mode, a.model = "ask", "small"
	a.mu.Unlock()
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id), Modes: fakeModes("ask"), ConfigOptions: a.configOptions()}, nil
}

func fakeModes(current string) *sdk.SessionModeState {
	desc := "Edits files"
	return &sdk.SessionModeState{CurrentModeId: sdk.SessionModeId(current), AvailableModes: []sdk.SessionMode{
		{Id: "ask", Name: "Ask"}, {Id: "code", Name: "Code", Description: &desc},
	}}
}

func (a *agent) configOptions() []sdk.SessionConfigOption {
	var opts []sdk.SessionConfigOption
	raw := fmt.Sprintf(`[{"type":"select","id":"model","name":"Model","currentValue":%q,"options":[{"value":"small","name":"Small"},{"value":"large","name":"Large"}]}]`, a.model)
	_ = json.Unmarshal([]byte(raw), &opts)
	return opts
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.ValueId != nil && p.ValueId.ConfigId == "model" {
		a.model = string(p.ValueId.Value)
	}
	return sdk.SetSessionConfigOptionResponse{ConfigOptions: a.configOptions()}, nil
}

func (a *agent) SetSessionMode(_ context.Context, p sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	a.mu.Lock()
	a.mode = string(p.ModeId)
	a.mu.Unlock()
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
		if text == "mode?" {
			a.mu.Lock()
			m, model := a.mode, a.model
			a.mu.Unlock()
			a.send(ctx, sid, sdk.UpdateAgentMessageText("mode: "+m+" model: "+model))
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		}
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

	case "escape":
		// Acts like an agent using its own tools without asking: writes files
		// itself and opens connections. The prompt is "write:a,b dial:host:port".
		var report []string
		for _, f := range strings.Fields(text) {
			switch {
			case strings.HasPrefix(f, "write:"):
				for _, p := range strings.Split(strings.TrimPrefix(f, "write:"), ",") {
					if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
						report = append(report, "write "+filepath.Base(p)+": denied")
					} else {
						report = append(report, "write "+filepath.Base(p)+": ok")
					}
				}
			case strings.HasPrefix(f, "dial:"):
				if c, err := net.DialTimeout("tcp", strings.TrimPrefix(f, "dial:"), 2*time.Second); err != nil {
					report = append(report, "dial: denied")
				} else {
					c.Close()
					report = append(report, "dial: ok")
				}
			}
		}
		a.send(ctx, sid, sdk.UpdateAgentMessageText(strings.Join(report, "; ")))
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "writedirect":
		// Writes without asking permission first, as a careless agent might.
		_, err := a.conn.WriteTextFile(ctx, sdk.WriteTextFileRequest{SessionId: sid, Path: filepath.Join(cwd, "direct.txt"), Content: "x"})
		if err != nil {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("write refused: "+err.Error()))
		} else {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("write done"))
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "leak":
		secret := os.Getenv("RBRIDGE_FAKE_SECRET_TOKEN")
		a.send(ctx, sid, sdk.UpdateAgentMessageText("the key is "+secret))
		fmt.Fprintln(os.Stderr, "fatal: request failed with key "+secret)
		os.Exit(4)

	case "terminal":
		// Runs the prompt as a shell command through the client's terminal.
		out, err := a.runTerminal(ctx, sid, "sh", []string{"-c", text}, nil)
		if err != nil {
			a.send(ctx, sid, sdk.UpdateAgentMessageText("terminal refused: "+err.Error()))
		} else {
			a.send(ctx, sid, sdk.UpdateAgentMessageText(out))
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

	case "elicit":
		var req sdk.CreateElicitationRequest
		raw := fmt.Sprintf(`{"mode":"form","sessionId":%q,"message":"What is your name?","requestedSchema":{"type":"object","properties":{"name":{"type":"string","title":"Name"}},"required":["name"]}}`, sid)
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			return sdk.PromptResponse{}, err
		}
		r, err := a.conn.CreateElicitation(ctx, req)
		switch {
		case err != nil:
			a.send(ctx, sid, sdk.UpdateAgentMessageText("elicitation error: "+err.Error()))
		case r.Accept != nil:
			b, _ := json.Marshal(r.Accept.Content)
			var content map[string]any
			json.Unmarshal(b, &content)
			a.send(ctx, sid, sdk.UpdateAgentMessageText(fmt.Sprint("hello ", content["name"])))
		case r.Cancel != nil:
			a.send(ctx, sid, sdk.UpdateAgentMessageText("cancelled"))
		default:
			a.send(ctx, sid, sdk.UpdateAgentMessageText("declined"))
		}
		return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil

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

// runTerminal creates a terminal, waits for it, and returns "exit=<code> <output>".
func (a *agent) runTerminal(ctx context.Context, sid sdk.SessionId, cmd string, args []string, limit *int64) (string, error) {
	tr, err := a.conn.CreateTerminal(ctx, sdk.CreateTerminalRequest{SessionId: sid, Command: cmd, Args: args, OutputByteLimit: limit})
	if err != nil {
		return "", err
	}
	w, err := a.conn.WaitForTerminalExit(ctx, sdk.WaitForTerminalExitRequest{SessionId: sid, TerminalId: tr.TerminalId})
	if err != nil {
		return "", err
	}
	out, err := a.conn.TerminalOutput(ctx, sdk.TerminalOutputRequest{SessionId: sid, TerminalId: tr.TerminalId})
	if err != nil {
		return "", err
	}
	_, _ = a.conn.ReleaseTerminal(ctx, sdk.ReleaseTerminalRequest{SessionId: sid, TerminalId: tr.TerminalId})
	status := "exit="
	switch {
	case w.ExitCode != nil:
		status += strconv.FormatInt(*w.ExitCode, 10)
	case w.Signal != nil:
		status = "signal=" + *w.Signal
	}
	if out.Truncated {
		status += " truncated"
	}
	return status + " " + out.Output, nil
}
