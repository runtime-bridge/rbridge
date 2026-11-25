// Package modelserver runs text-only tasks on a local model server, such as
// Ollama or an OpenAI-compatible server. It is not an agent: it has no tools,
// cannot touch files or run commands, and refuses skills and MCP servers.
package modelserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/runtime-bridge/rbridge/internal/domain"
)

// API is the wire format a server speaks.
type API string

const (
	APIOllama API = "ollama" // /api/chat, newline-delimited JSON
	APIOpenAI API = "openai" // /v1/chat/completions, server-sent events
)

// Server is where and how to reach a model server.
type Server struct {
	URL string
	API API
}

// NormalizeURL turns a configured address into a base URL. A bare host:port
// (as OLLAMA_HOST allows) gets http://, and 0.0.0.0 becomes 127.0.0.1.
// Only loopback addresses are accepted: a model server provider is local.
func NormalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("no server address")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("%q is not an http(s) address", raw)
	}
	host := u.Hostname()
	if host == "0.0.0.0" || host == "" {
		host = "127.0.0.1"
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("%s is not on this machine; model servers must listen on a loopback address", host)
	}
	port := u.Port()
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host + strings.TrimSuffix(u.Path, "/"), nil
}

// Probe asks the server for its version or model list. It reports whether
// the server answered, its version when known, and a note.
func Probe(ctx context.Context, client *http.Client, s Server) (ok bool, version, note string) {
	path := "/api/version"
	if s.API == APIOpenAI {
		path = "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	if err != nil {
		return false, "", err.Error()
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, "", "no server answers at " + s.URL
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Sprintf("%s answered HTTP %d", s.URL+path, resp.StatusCode)
	}
	var v struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v)
	return true, v.Version, ""
}

// Models lists the models the server has.
func Models(ctx context.Context, client *http.Client, s Server) ([]string, error) {
	path := "/api/tags"
	if s.API == APIOpenAI {
		path = "/v1/models"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	var out []string
	for _, m := range body.Models {
		out = append(out, m.Name)
	}
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Session is a conversation with a model. The history lives in memory, so
// a session cannot be resumed after it closes.
type Session struct {
	server Server
	client *http.Client
	opts   domain.SessionOptions
	sink   domain.EventSink
	id     string

	mu      sync.Mutex
	history []message
	cancel  context.CancelFunc
	closed  bool
}

// Open checks the request and the server and starts a session. It sends no
// prompt.
func Open(ctx context.Context, server Server, client *http.Client, caps domain.CapabilitySet, opts domain.SessionOptions, sink domain.EventSink) (*Session, error) {
	if len(opts.MCPServers) > 0 {
		return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "a model server has no tools, so it cannot use MCP servers")
	}
	if len(opts.SkillDirs) > 0 {
		return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "a model server cannot use skills")
	}
	if opts.ResumeNativeID != "" {
		return nil, domain.Errorf(domain.ErrCapabilityUnavailable, "model server sessions cannot be resumed")
	}
	if client == nil {
		client = &http.Client{}
	}
	if ok, _, note := Probe(ctx, client, server); !ok {
		return nil, domain.Errorf(domain.ErrProviderUnavailable, "%s", note)
	}
	if opts.Model == "" {
		models, _ := Models(ctx, client, server)
		msg := "choose a model with --model or providers.<id>.model"
		if len(models) > 0 {
			msg += "; this server has: " + strings.Join(models, ", ")
		} else {
			msg += "; this server reports no models"
		}
		return nil, domain.Errorf(domain.ErrInvalidInput, "%s", msg)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := &Session{server: server, client: client, opts: opts, sink: sink, id: "local-" + hex.EncodeToString(b[:])}
	s.emit(domain.NewEvent(domain.EventSessionStarted, domain.SessionStartedData{NativeID: s.id, Model: opts.Model, AuthMode: domain.AuthLocal}))
	return s, nil
}

func (s *Session) emit(e domain.Event) {
	if s.sink != nil {
		s.sink.Emit(e)
	}
}

func (s *Session) NativeID() string { return s.id }

func (s *Session) Capabilities() domain.CapabilitySet { return Capabilities() }

// Capabilities are what every model server offers through rbridge.
func Capabilities() domain.CapabilitySet {
	c := func(sup domain.Support, mode string) domain.Capability {
		return domain.Capability{Supported: sup, Mode: mode, Source: domain.SourceManifest}
	}
	return domain.CapabilitySet{
		domain.CapStreaming:          c(domain.SupportYes, "streamed tokens"),
		domain.CapSessionsPersistent: c(domain.SupportYes, "history kept by rbridge for the session"),
		domain.CapSessionsResume:     c(domain.SupportNo, ""),
		domain.CapSessionsLoad:       c(domain.SupportNo, ""),
		domain.CapCancel:             c(domain.SupportYes, "closes the request"),
		domain.CapSkills:             c(domain.SupportNo, "text generation only"),
		domain.CapMCPStdio:           c(domain.SupportNo, ""),
		domain.CapFilesystem:         c(domain.SupportNo, ""),
		domain.CapShell:              c(domain.SupportNo, ""),
		domain.CapApprovals:          c(domain.SupportNo, "no tools, nothing to approve"),
		domain.CapStructuredOutput:   c(domain.SupportPartial, "JSON schema passed to the server; models may not follow it"),
		domain.CapPromptImage:        c(domain.SupportNo, ""),
		domain.CapOffline:            c(domain.SupportYes, "local model"),
		domain.CapUsage:              c(domain.SupportYes, "token counts from the server"),
	}
}

// Prompt sends the conversation so far plus this turn and streams the reply.
func (s *Session) Prompt(ctx context.Context, turn domain.Turn) (domain.TurnResult, error) {
	var text strings.Builder
	for _, b := range turn.Prompt {
		switch b.Type {
		case "text":
			if text.Len() > 0 {
				text.WriteString("\n\n")
			}
			text.WriteString(b.Text)
		default:
			return domain.TurnResult{}, domain.Errorf(domain.ErrCapabilityUnavailable, "a model server takes text only, not %s", b.Type)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return domain.TurnResult{}, domain.Errorf(domain.ErrSessionNotFound, "session is closed")
	}
	s.cancel = cancel
	msgs := append(append([]message(nil), s.history...), message{Role: "user", Content: text.String()})
	s.mu.Unlock()

	reply, usage, err := s.chat(ctx, msgs)
	s.mu.Lock()
	s.cancel = nil
	s.mu.Unlock()
	if ctx.Err() != nil {
		return domain.TurnResult{StopReason: domain.StopCancelled, Output: reply}, nil
	}
	if err != nil {
		return domain.TurnResult{}, err
	}
	s.mu.Lock()
	s.history = append(msgs, message{Role: "assistant", Content: reply})
	s.mu.Unlock()
	res := domain.TurnResult{StopReason: domain.StopEndTurn, Output: reply, Usage: usage, AuthMode: domain.AuthLocal}
	if len(s.opts.OutputSchema) > 0 {
		trimmed := strings.TrimSpace(reply)
		if json.Valid([]byte(trimmed)) {
			res.Structured = json.RawMessage(trimmed)
		}
	}
	return res, nil
}

func (s *Session) chat(ctx context.Context, msgs []message) (string, *domain.Usage, error) {
	body := map[string]any{"model": s.opts.Model, "messages": msgs, "stream": true}
	path := "/api/chat"
	if s.server.API == APIOpenAI {
		path = "/v1/chat/completions"
		body["stream_options"] = map[string]any{"include_usage": true}
		if len(s.opts.OutputSchema) > 0 {
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "result", "schema": s.opts.OutputSchema}}
		}
	} else if len(s.opts.OutputSchema) > 0 {
		body["format"] = s.opts.OutputSchema
	}
	b, err := json.Marshal(body)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.server.URL+path, bytes.NewReader(b))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, &domain.Error{Code: domain.ErrProviderUnavailable, Message: "model server: " + err.Error(), Retryable: true}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		code := domain.ErrExecutionFailed
		if resp.StatusCode == http.StatusNotFound {
			code = domain.ErrInvalidInput
		}
		return "", nil, domain.Errorf(code, "model server answered HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var out strings.Builder
	var usage *domain.Usage
	idle := s.opts.IdleTimeout
	if idle <= 0 {
		idle = 5 * time.Minute
	}
	lines := make(chan string)
	scanErr := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		scanErr <- sc.Err()
		close(lines)
	}()
	timer := time.NewTimer(idle)
	defer timer.Stop()
	for {
		var line string
		var more bool
		select {
		case line, more = <-lines:
		case <-timer.C:
			return out.String(), usage, domain.Errorf(domain.ErrExecutionTimeout, "model server sent nothing for %s", idle)
		case <-ctx.Done():
			return out.String(), usage, ctx.Err()
		}
		if !more {
			if err := <-scanErr; err != nil && ctx.Err() == nil {
				return out.String(), usage, domain.Errorf(domain.ErrProviderProtocol, "model server stream: %v", err)
			}
			return out.String(), usage, nil
		}
		timer.Reset(idle)
		delta, u, done, err := s.parse(line)
		if err != nil {
			return out.String(), usage, err
		}
		if delta != "" {
			out.WriteString(delta)
			s.emit(domain.NewEvent(domain.EventMessageDelta, domain.TextData{Text: delta}))
		}
		if u != nil {
			usage = u
			s.emit(domain.NewEvent(domain.EventUsageUpdated, *u))
		}
		if done {
			return out.String(), usage, nil
		}
	}
}

// parse reads one line of the stream.
func (s *Session) parse(line string) (delta string, usage *domain.Usage, done bool, err error) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", nil, false, nil
	}
	if s.server.API == APIOpenAI {
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			return "", nil, false, nil
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			return "", nil, true, nil
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int64 `json:"prompt_tokens"`
				Completion int64 `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return "", nil, false, domain.Errorf(domain.ErrProviderProtocol, "model server sent an unreadable event: %v", err)
		}
		if c.Error != nil {
			return "", nil, false, domain.Errorf(domain.ErrExecutionFailed, "model server: %s", c.Error.Message)
		}
		for _, ch := range c.Choices {
			delta += ch.Delta.Content
		}
		if c.Usage != nil {
			usage = &domain.Usage{InputTokens: c.Usage.Prompt, OutputTokens: c.Usage.Completion}
		}
		return delta, usage, false, nil
	}
	var o struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Done       bool   `json:"done"`
		Error      string `json:"error"`
		PromptEval int64  `json:"prompt_eval_count"`
		Eval       int64  `json:"eval_count"`
	}
	if err := json.Unmarshal([]byte(line), &o); err != nil {
		return "", nil, false, domain.Errorf(domain.ErrProviderProtocol, "model server sent an unreadable line: %v", err)
	}
	if o.Error != "" {
		return "", nil, false, domain.Errorf(domain.ErrExecutionFailed, "model server: %s", o.Error)
	}
	if o.Done {
		usage = &domain.Usage{InputTokens: o.PromptEval, OutputTokens: o.Eval}
	}
	return o.Message.Content, usage, o.Done, nil
}

// Cancel stops the current turn by closing its request.
func (s *Session) Cancel(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Close ends the session. There is no process to stop.
func (s *Session) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

var _ domain.ProviderSession = (*Session)(nil)
