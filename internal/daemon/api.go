package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/runtime-bridge/rbridge/internal/skills/catalog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/runtime-bridge/rbridge/internal/buildinfo"
	"github.com/runtime-bridge/rbridge/internal/domain"
	"github.com/runtime-bridge/rbridge/internal/service"
	skillreg "github.com/runtime-bridge/rbridge/internal/skills/registry"
)

// API is the method set shared by every transport.
type API struct {
	Hub     *Hub
	Svc     *service.Service
	Started time.Time
}

// Method handles one call. params is the JSON object sent by the client.
type Method func(ctx context.Context, owner string, params json.RawMessage) (any, error)

var clientPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidClient reports whether a client name is acceptable.
func ValidClient(s string) bool { return clientPattern.MatchString(s) }

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 || string(params) == "null" {
		return nil
	}
	if err := json.Unmarshal(params, v); err != nil {
		return domain.Errorf(domain.ErrInvalidInput, "invalid parameters: %v", err)
	}
	return nil
}

// Prompt accepts a plain string or a list of content blocks.
type Prompt []domain.ContentBlock

func (p *Prompt) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*p = domain.Text(s)
		return nil
	}
	var blocks []domain.ContentBlock
	if err := json.Unmarshal(b, &blocks); err != nil {
		return errors.New("prompt must be a string or a list of content blocks")
	}
	*p = blocks
	return nil
}

type idParams struct {
	ID      string `json:"id"`
	Refresh bool   `json:"refresh"`
	Model   string `json:"model"`
	Input   bool   `json:"input"`
}

type modeParams struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type optionParams struct {
	ID       string `json:"id"`
	OptionID string `json:"option_id"`
	Value    any    `json:"value"`
}

type inputParams struct {
	SessionID string         `json:"session_id"`
	InputID   string         `json:"input_id"`
	Action    string         `json:"action"`
	Content   map[string]any `json:"content"`
}

type promptParams struct {
	ID     string `json:"id"`
	Prompt Prompt `json:"prompt"`
	// Wait returns the result when the turn ends; otherwise the call returns
	// at once and the outcome arrives as events.
	Wait *bool `json:"wait"`
}

type answerParams struct {
	SessionID  string `json:"session_id"`
	ApprovalID string `json:"approval_id"`
	Allow      bool   `json:"allow"`
	OptionID   string `json:"option_id"`
	Remember   bool   `json:"remember"`
}

type skillAddParams struct {
	Path   string `json:"path"`
	Link   bool   `json:"link"`
	URL    string `json:"url"`
	Rev    string `json:"rev"`
	Subdir string `json:"subdir"`
	// Catalog installs "catalog:id" or "id" from a configured catalog.
	Catalog string `json:"catalog"`
}

type trustParams struct {
	ID    string `json:"id"`
	Trust string `json:"trust"`
}

type auditParams struct {
	Since time.Time `json:"since"`
}

// Status is returned by daemon.status.
type Status struct {
	Version  string    `json:"version"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started_at"`
	Sessions int       `json:"sessions_open"`
}

// Methods returns the method table.
func (a *API) Methods() map[string]Method {
	return map[string]Method{
		"daemon.status": func(context.Context, string, json.RawMessage) (any, error) {
			return Status{Version: buildinfo.Get().Version, PID: os.Getpid(), Started: a.Started, Sessions: a.Hub.Count()}, nil
		},
		"providers.list": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Svc.Providers(ctx, in.Refresh)
		},
		"providers.get": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Svc.Provider(ctx, in.ID, in.Refresh)
		},
		"skills.list": func(ctx context.Context, _ string, _ json.RawMessage) (any, error) {
			list, err := a.Svc.Skills().List(ctx)
			if list == nil {
				list = []domain.SkillRecord{}
			}
			return list, err
		},
		"skills.get": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Svc.Skills().Require(ctx, in.ID)
		},
		"skills.add": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in skillAddParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			if in.Path != "" && !filepath.IsAbs(in.Path) {
				return nil, domain.Errorf(domain.ErrInvalidInput, "path must be absolute")
			}
			if in.Catalog != "" {
				return a.Svc.AddFromCatalog(ctx, in.Catalog)
			}
			return a.Svc.Skills().Add(ctx, skillreg.AddRequest{Path: in.Path, Link: in.Link, URL: in.URL, Rev: in.Rev, Subdir: in.Subdir})
		},
		"skills.add_many": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in struct {
				Paths   []string `json:"paths"`
				Link    bool     `json:"link"`
				URL     string   `json:"url"`
				Rev     string   `json:"rev"`
				Subdirs []string `json:"subdirs"`
				Only    []string `json:"only"`
				Exclude []string `json:"exclude"`
				DryRun  bool     `json:"dry_run"`
				Trust   string   `json:"trust"`
			}
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			for _, path := range in.Paths {
				if !filepath.IsAbs(path) {
					return nil, domain.Errorf(domain.ErrInvalidInput, "paths must be absolute")
				}
			}
			req := skillreg.AddManyRequest{Paths: in.Paths, Link: in.Link, URL: in.URL, Rev: in.Rev, Subdirs: in.Subdirs, Only: in.Only, Exclude: in.Exclude, DryRun: in.DryRun}
			if in.Trust != "" {
				state, err := skillreg.ParseTrust(in.Trust)
				if err != nil {
					return nil, err
				}
				req.SetTrust = func(ctx context.Context, id string) (domain.SkillRecord, error) {
					return a.Svc.SetSkillTrust(ctx, id, state)
				}
			}
			res, err := a.Svc.Skills().AddMany(ctx, req)
			if err != nil {
				return nil, err
			}
			if res.Items == nil {
				res.Items = []skillreg.AddManyItem{}
			}
			return res, nil
		},
		"skills.search": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in struct {
				Query string `json:"query"`
			}
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			entries, problems, err := a.Svc.SearchCatalogs(ctx, in.Query)
			if err != nil {
				return nil, err
			}
			if entries == nil {
				entries = []catalog.Entry{}
			}
			if problems == nil {
				problems = []string{}
			}
			return map[string]any{"skills": entries, "problems": problems}, nil
		},
		"skills.trust": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in trustParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			t, err := skillreg.ParseTrust(in.Trust)
			if err != nil {
				return nil, err
			}
			return a.Svc.SetSkillTrust(ctx, in.ID, t)
		},
		"skills.remove": func(ctx context.Context, _ string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return map[string]bool{"removed": true}, a.Svc.RemoveSkill(ctx, in.ID)
		},
		"sessions.open": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in OpenRequest
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			if len(in.Task.Prompt) == 0 {
				in.Task.Prompt = domain.Text("session")
			}
			return a.Hub.Open(ctx, owner, in)
		},
		"sessions.resume": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.Resume(ctx, owner, in.ID, in.Model, in.Input)
		},
		"sessions.list": func(ctx context.Context, owner string, _ json.RawMessage) (any, error) {
			return a.Hub.List(ctx, owner)
		},
		"sessions.get": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.Get(ctx, owner, in.ID)
		},
		"sessions.prompt": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in promptParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			if len(in.Prompt) == 0 {
				return nil, domain.Errorf(domain.ErrInvalidInput, "prompt is required")
			}
			if in.Wait != nil && !*in.Wait {
				return map[string]bool{"started": true}, a.Hub.Start(owner, in.ID, in.Prompt)
			}
			return a.Hub.Prompt(ctx, owner, in.ID, in.Prompt)
		},
		"sessions.cancel": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return map[string]bool{"cancelled": true}, a.Hub.Cancel(ctx, owner, in.ID)
		},
		"sessions.close": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return map[string]bool{"closed": true}, a.Hub.Close(ctx, owner, in.ID)
		},
		"sessions.approvals": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.Pending(owner, in.ID)
		},
		"sessions.modes": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.Modes(owner, in.ID)
		},
		"sessions.set_mode": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in modeParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			if err := a.Hub.SetMode(ctx, owner, in.ID, in.Mode); err != nil {
				return nil, err
			}
			return a.Hub.Modes(owner, in.ID)
		},
		"sessions.options": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.Options(owner, in.ID)
		},
		"sessions.set_option": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in optionParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			var value string
			switch v := in.Value.(type) {
			case string:
				value = v
			case bool:
				value = fmt.Sprint(v)
			default:
				return nil, domain.Errorf(domain.ErrInvalidInput, "value must be a string or a boolean")
			}
			return a.Hub.SetOption(ctx, owner, in.ID, in.OptionID, value)
		},
		"sessions.inputs": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in idParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return a.Hub.PendingInputs(owner, in.ID)
		},
		"inputs.answer": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in inputParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return map[string]bool{"answered": true}, a.Hub.AnswerInput(owner, in.SessionID, in.InputID, in.Action, in.Content)
		},
		"approvals.answer": func(_ context.Context, owner string, p json.RawMessage) (any, error) {
			var in answerParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			return map[string]bool{"answered": true}, a.Hub.Answer(owner, in.SessionID, in.ApprovalID, in.Allow, in.OptionID, in.Remember)
		},
		"run": func(ctx context.Context, owner string, p json.RawMessage) (any, error) {
			var in OpenRequest
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			rec, err := a.Hub.Open(ctx, owner, in)
			if err != nil {
				return nil, err
			}
			defer a.Hub.Close(context.Background(), owner, rec.ID)
			return a.Hub.Prompt(ctx, owner, rec.ID, in.Task.Prompt)
		},
		"audit.list": func(_ context.Context, _ string, p json.RawMessage) (any, error) {
			var in auditParams
			if err := decode(p, &in); err != nil {
				return nil, err
			}
			recs, err := a.Svc.Audit().Read(in.Since)
			if recs == nil && err == nil {
				return []any{}, nil
			}
			return recs, err
		},
	}
}

// ErrorBody is the error shape on every transport.
type ErrorBody struct {
	Code    domain.ErrorCode  `json:"code"`
	Message string            `json:"message"`
	Details map[string]string `json:"details,omitempty"`
}

func errorBody(err error) ErrorBody {
	var de *domain.Error
	if errors.As(err, &de) {
		return ErrorBody{Code: de.Code, Message: de.Message, Details: de.Details}
	}
	return ErrorBody{Code: domain.ErrInternal, Message: err.Error()}
}

// HTTPStatus maps an error code to an HTTP status.
func HTTPStatus(code domain.ErrorCode) int {
	switch code {
	case domain.ErrInvalidInput, domain.ErrSkillInvalid:
		return http.StatusBadRequest
	case domain.ErrProviderNotFound, domain.ErrSessionNotFound, domain.ErrSkillNotFound:
		return http.StatusNotFound
	case domain.ErrCapabilityUnavailable, domain.ErrSkillBlocked, domain.ErrSkillUnavailable, domain.ErrSkillDigestMismatch,
		domain.ErrPolicyUnenforceable, domain.ErrPermissionDenied, domain.ErrCancelled:
		return http.StatusConflict
	case domain.ErrAuthenticationRequired:
		return http.StatusFailedDependency
	case domain.ErrProviderUnavailable:
		return http.StatusServiceUnavailable
	case domain.ErrExecutionFailed, domain.ErrProviderProtocol:
		return http.StatusBadGateway
	case domain.ErrExecutionTimeout, domain.ErrApprovalTimeout:
		return http.StatusGatewayTimeout
	}
	return http.StatusInternalServerError
}
