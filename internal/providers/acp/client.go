package acp

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

// client serves the requests an ACP agent sends to rbridge.
type client struct{ s *Session }

var _ sdk.Client = (*client)(nil)

func raw(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func (c *client) SessionUpdate(_ context.Context, n sdk.SessionNotification) error {
	s := c.s
	u := n.Update
	switch {
	case u.AgentMessageChunk != nil:
		if t := u.AgentMessageChunk.Content.Text; t != nil {
			s.mu.Lock()
			s.text.WriteString(t.Text)
			s.mu.Unlock()
			s.emit(domain.NewEvent(domain.EventMessageDelta, domain.TextData{Text: t.Text}).WithRaw(raw(u.AgentMessageChunk)))
		}
	case u.AgentThoughtChunk != nil:
		if t := u.AgentThoughtChunk.Content.Text; t != nil {
			s.emit(domain.NewEvent(domain.EventThoughtDelta, domain.TextData{Text: t.Text}).WithRaw(raw(u.AgentThoughtChunk)))
		}
	case u.ToolCall != nil:
		tc := u.ToolCall
		s.emit(domain.NewEvent(domain.EventToolStarted, domain.ToolData{
			ID: string(tc.ToolCallId), Title: tc.Title, Kind: string(tc.Kind), Status: string(tc.Status), Input: tc.RawInput,
		}).WithRaw(raw(tc)))
	case u.ToolCallUpdate != nil:
		tu := u.ToolCallUpdate
		d := domain.ToolData{ID: string(tu.ToolCallId)}
		if v, ok := tu.Title.Value(); ok {
			d.Title = v
		}
		if v, ok := tu.Kind.Value(); ok {
			d.Kind = string(v)
		}
		if v, ok := tu.Status.Value(); ok {
			d.Status = string(v)
		}
		s.emit(domain.NewEvent(domain.EventToolUpdated, d).WithRaw(raw(tu)))
	case u.Plan != nil:
		var d domain.PlanData
		for _, e := range u.Plan.Entries {
			d.Entries = append(d.Entries, domain.PlanEntry{Content: e.Content, Status: string(e.Status), Priority: string(e.Priority)})
		}
		s.emit(domain.NewEvent(domain.EventPlanUpdated, d).WithRaw(raw(u.Plan)))
	case u.UsageUpdate != nil:
		d := map[string]any{"context_used": u.UsageUpdate.Used, "context_size": u.UsageUpdate.Size}
		if cst := u.UsageUpdate.Cost; cst != nil {
			d["cost"] = cst.Amount
			d["currency"] = cst.Currency
		}
		s.emit(domain.NewEvent(domain.EventUsageUpdated, d).WithRaw(raw(u.UsageUpdate)))
	case u.Notice != nil:
		msg := u.Notice.Title
		if u.Notice.Description != nil {
			msg += ": " + *u.Notice.Description
		}
		s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Message: msg, Level: string(u.Notice.Severity)}).WithRaw(raw(u.Notice)))
	case u.CurrentModeUpdate != nil:
		s.mu.Lock()
		s.modeID = string(u.CurrentModeUpdate.CurrentModeId)
		s.mu.Unlock()
		s.emit(domain.NewEvent(domain.EventModeUpdated, domain.ModeUpdatedData{Mode: string(u.CurrentModeUpdate.CurrentModeId)}).WithRaw(raw(u.CurrentModeUpdate)))
	case u.ConfigOptionUpdate != nil:
		s.setOptions(u.ConfigOptionUpdate.ConfigOptions)
	case u.AvailableCommandsUpdate != nil, u.UserMessageChunk != nil, u.SessionInfoUpdate != nil:
		// Session bookkeeping; not part of the bridge event stream.
	default:
		b, _ := json.Marshal(u)
		s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Message: "unmapped session update", Level: "info"}).WithRaw(b))
	}
	return nil
}

func (c *client) RequestPermission(ctx context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	s := c.s
	req := domain.ApprovalRequest{
		ID:        string(p.ToolCall.ToolCallId),
		SessionID: s.opts.BridgeID,
	}
	if v, ok := p.ToolCall.Title.Value(); ok {
		req.Title = v
	}
	if v, ok := p.ToolCall.Kind.Value(); ok {
		req.Kind = string(v)
	}
	if v, ok := p.ToolCall.Name.Value(); ok {
		req.Tool = v
	}
	if v, ok := p.ToolCall.RawInput.Value(); ok {
		req.Input = v
	}
	for _, o := range p.Options {
		req.Options = append(req.Options, domain.ApprovalOption{ID: string(o.OptionId), Kind: string(o.Kind), Name: o.Name})
	}
	var d domain.ApprovalDecision
	if s.opts.Approver == nil {
		d = req.RejectOnce("policy")
		d.Reason = "no approver configured"
	} else {
		d = s.opts.Approver.Decide(ctx, req)
	}
	s.touch()
	if d.OptionID == "" {
		return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{Cancelled: &sdk.RequestPermissionOutcomeCancelled{}}}, nil
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: sdk.PermissionOptionId(d.OptionID)}}}, nil
}

// insideWorkspace resolves symlinks in the existing part of p and checks the
// result stays under the workspace.
func (c *client) insideWorkspace(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", sdk.NewInvalidParams(map[string]string{"error": "path must be absolute"})
	}
	root, err := filepath.EvalSymlinks(c.s.opts.Workspace)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(p)
	existing, rest := clean, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	full := filepath.Join(resolved, rest)
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", sdk.NewRequestError(-32602, "path is outside the workspace", map[string]string{"path": p})
	}
	return full, nil
}

func (c *client) gate(ctx context.Context, kind, path string) error {
	if c.s.opts.FSGate == nil {
		return nil
	}
	if err := c.s.opts.FSGate(ctx, kind, path); err != nil {
		return sdk.NewRequestError(-32001, err.Error(), map[string]string{"path": path})
	}
	return nil
}

func (c *client) ReadTextFile(ctx context.Context, p sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	path, err := c.insideWorkspace(p.Path)
	if err != nil {
		return sdk.ReadTextFileResponse{}, err
	}
	if err := c.gate(ctx, "read", path); err != nil {
		return sdk.ReadTextFileResponse{}, err
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return sdk.ReadTextFileResponse{}, sdk.NewRequestError(-32002, "file not found", map[string]string{"path": p.Path})
	}
	if err != nil {
		return sdk.ReadTextFileResponse{}, err
	}
	text := string(b)
	if p.Line != nil || p.Limit != nil {
		lines := strings.SplitAfter(text, "\n")
		start := 0
		if p.Line != nil && *p.Line > 1 {
			start = int(*p.Line) - 1
		}
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		if p.Limit != nil && start+int(*p.Limit) < end {
			end = start + int(*p.Limit)
		}
		text = strings.Join(lines[start:end], "")
	}
	return sdk.ReadTextFileResponse{Content: text}, nil
}

func (c *client) WriteTextFile(ctx context.Context, p sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	if !c.s.opts.ClientFSWrite {
		return sdk.WriteTextFileResponse{}, sdk.NewRequestError(-32601, "file writes are not offered in this session", nil)
	}
	path, err := c.insideWorkspace(p.Path)
	if err != nil {
		return sdk.WriteTextFileResponse{}, err
	}
	if err := c.gate(ctx, "edit", path); err != nil {
		return sdk.WriteTextFileResponse{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return sdk.WriteTextFileResponse{}, err
	}
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(p.Content), mode); err != nil {
		return sdk.WriteTextFileResponse{}, err
	}
	c.s.emit(domain.NewEvent(domain.EventNotice, domain.NoticeData{Message: "runtime wrote " + p.Path + " through rbridge", Level: "info"}))
	return sdk.WriteTextFileResponse{}, nil
}

func errTerminal() error {
	return sdk.NewRequestError(-32601, "rbridge does not offer terminals to this session", nil)
}

// CreateElicitation passes a question from the agent to the caller. URLs are
// never opened by rbridge; the caller decides what to do with them.
func (c *client) CreateElicitation(ctx context.Context, p sdk.CreateElicitationRequest) (sdk.CreateElicitationResponse, error) {
	s := c.s
	if s.opts.Elicitor == nil {
		return sdk.CreateElicitationResponse{Decline: &sdk.CreateElicitationDecline{Action: "decline"}}, nil
	}
	req := domain.InputRequest{SessionID: s.opts.BridgeID}
	switch {
	case p.Form != nil:
		req.Mode, req.Message = "form", p.Form.Message
		req.Schema, _ = json.Marshal(p.Form.RequestedSchema)
	case p.Url != nil:
		req.Mode, req.Message, req.URL = "url", p.Url.Message, p.Url.Url
		req.ID = string(p.Url.ElicitationId)
	default:
		return sdk.CreateElicitationResponse{Decline: &sdk.CreateElicitationDecline{Action: "decline"}}, nil
	}
	if req.ID == "" {
		req.ID = newTerminalID()[len("term-"):]
	}
	s.emit(domain.NewEvent(domain.EventInputRequired, req))
	resp := s.opts.Elicitor.Ask(ctx, req)
	s.touch()
	by := "user"
	if resp.Action == "" {
		resp.Action = "decline"
	}
	s.emit(domain.NewEvent(domain.EventInputResolved, domain.InputResolvedData{ID: req.ID, Action: resp.Action, By: by}))
	switch resp.Action {
	case "accept":
		out := sdk.CreateElicitationResponse{Accept: &sdk.CreateElicitationAccept{Action: "accept"}}
		if len(resp.Content) > 0 {
			b, err := json.Marshal(resp.Content)
			if err != nil {
				return sdk.CreateElicitationResponse{}, err
			}
			var content map[string]sdk.ElicitationContentValue
			if err := json.Unmarshal(b, &content); err != nil {
				return sdk.CreateElicitationResponse{}, sdk.NewInvalidParams(map[string]string{"error": "answer does not fit the form: " + err.Error()})
			}
			out.Accept.Content = content
		}
		return out, nil
	case "cancel":
		return sdk.CreateElicitationResponse{Cancel: &sdk.CreateElicitationCancel{Action: "cancel"}}, nil
	}
	return sdk.CreateElicitationResponse{Decline: &sdk.CreateElicitationDecline{Action: "decline"}}, nil
}

func (c *client) CompleteElicitation(context.Context, sdk.CompleteElicitationNotification) error {
	return nil
}
