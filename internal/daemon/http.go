package daemon

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rbridge/rbridge/internal/domain"
)

// HTTPOptions configure the HTTP handler.
type HTTPOptions struct {
	Token          string
	AllowedOrigins []string
	// CheckHost restricts the Host header to loopback names, which defeats
	// DNS rebinding from web pages.
	CheckHost         bool
	RequestsPerSecond float64
	MaxBody           int64
}

type httpAPI struct {
	api     *API
	methods map[string]Method
	opts    HTTPOptions
	limits  *limiter
}

// NewHTTPHandler returns the /v1 API handler.
func NewHTTPHandler(api *API, o HTTPOptions) http.Handler {
	if o.MaxBody <= 0 {
		o.MaxBody = 1 << 20
	}
	h := &httpAPI{api: api, methods: api.Methods(), opts: o, limits: newLimiter(o.RequestsPerSecond)}
	mux := http.NewServeMux()
	route := func(pattern, method string, status int, build func(r *http.Request, body json.RawMessage) (json.RawMessage, error)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) { h.call(w, r, method, status, build) })
	}
	pathID := func(keys ...string) func(*http.Request, json.RawMessage) (json.RawMessage, error) {
		return func(r *http.Request, body json.RawMessage) (json.RawMessage, error) {
			m := map[string]any{}
			if len(body) > 0 {
				if err := json.Unmarshal(body, &m); err != nil {
					return nil, domain.Errorf(domain.ErrInvalidInput, "body is not a JSON object")
				}
			}
			for _, k := range keys {
				m[k] = r.PathValue(k)
			}
			if r.URL.Query().Get("refresh") == "true" {
				m["refresh"] = true
			}
			return json.Marshal(m)
		}
	}
	plain := func(r *http.Request, body json.RawMessage) (json.RawMessage, error) { return pathID()(r, body) }

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	route("GET /v1/status", "daemon.status", 200, plain)
	route("GET /v1/providers", "providers.list", 200, plain)
	route("GET /v1/providers/{id}", "providers.get", 200, pathID("id"))
	route("GET /v1/skills", "skills.list", 200, plain)
	route("POST /v1/skills", "skills.add", 201, plain)
	route("GET /v1/skills/{id}", "skills.get", 200, pathID("id"))
	route("PUT /v1/skills/{id}/trust", "skills.trust", 200, pathID("id"))
	route("DELETE /v1/skills/{id}", "skills.remove", 200, pathID("id"))
	route("POST /v1/sessions", "sessions.open", 201, plain)
	route("GET /v1/sessions", "sessions.list", 200, plain)
	route("GET /v1/sessions/{id}", "sessions.get", 200, pathID("id"))
	route("DELETE /v1/sessions/{id}", "sessions.close", 200, pathID("id"))
	route("POST /v1/sessions/{id}/resume", "sessions.resume", 200, pathID("id"))
	route("POST /v1/sessions/{id}/turns", "sessions.prompt", 200, pathID("id"))
	route("POST /v1/sessions/{id}/cancel", "sessions.cancel", 200, pathID("id"))
	route("GET /v1/sessions/{id}/approvals", "sessions.approvals", 200, pathID("id"))
	route("POST /v1/sessions/{session_id}/approvals/{approval_id}", "approvals.answer", 200, pathID("session_id", "approval_id"))
	route("POST /v1/run", "run", 200, plain)
	route("GET /v1/audit", "audit.list", 200, func(r *http.Request, _ json.RawMessage) (json.RawMessage, error) {
		since := time.Time{}
		if s := r.URL.Query().Get("since"); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				return nil, domain.Errorf(domain.ErrInvalidInput, "since must be RFC 3339")
			}
			since = t
		}
		return json.Marshal(map[string]any{"since": since})
	})
	mux.HandleFunc("GET /v1/sessions/{id}/events", h.events)
	return h.guard(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]ErrorBody{"error": errorBody(err)})
}

func loopbackHost(host string) bool {
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// guard applies the checks every request must pass, in order: host, origin,
// token, rate.
func (h *httpAPI) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.opts.CheckHost && !loopbackHost(r.Host) {
			writeErr(w, http.StatusMisdirectedRequest, domain.Errorf(domain.ErrPermissionDenied, "host %q is not allowed", r.Host))
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !slices.Contains(h.opts.AllowedOrigins, o) {
			writeErr(w, http.StatusForbidden, domain.Errorf(domain.ErrPermissionDenied, "origin %q is not allowed", o))
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Vary", "Origin")
			if r.Method == http.MethodOptions {
				w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Rbridge-Client, Last-Event-ID")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		if r.URL.Path != "/v1/health" {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(h.opts.Token)) != 1 || h.opts.Token == "" {
				writeErr(w, http.StatusUnauthorized, domain.Errorf(domain.ErrPermissionDenied, "missing or wrong token"))
				return
			}
		}
		client := r.Header.Get("X-Rbridge-Client")
		if client == "" {
			client = "default"
		}
		if !ValidClient(client) {
			writeErr(w, http.StatusBadRequest, domain.Errorf(domain.ErrInvalidInput, "X-Rbridge-Client must be 1 to 64 letters, digits, dots, dashes or underscores"))
			return
		}
		if !h.limits.allow(client) {
			writeErr(w, http.StatusTooManyRequests, domain.Errorf(domain.ErrPermissionDenied, "too many requests"))
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, h.opts.MaxBody)
		next.ServeHTTP(w, r.WithContext(withClient(r.Context(), client)))
	})
}

type clientKey struct{}

func withClient(ctx context.Context, c string) context.Context {
	return context.WithValue(ctx, clientKey{}, c)
}

func clientOf(ctx context.Context) string {
	c, _ := ctx.Value(clientKey{}).(string)
	return c
}

func (h *httpAPI) call(w http.ResponseWriter, r *http.Request, method string, status int, build func(*http.Request, json.RawMessage) (json.RawMessage, error)) {
	var body json.RawMessage
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, domain.Errorf(domain.ErrInvalidInput, "request body too large"))
			return
		}
		body = b
	}
	params, err := build(r, body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := h.methods[method](r.Context(), clientOf(r.Context()), params)
	if err != nil {
		writeErr(w, HTTPStatus(domain.CodeOf(err)), err)
		return
	}
	writeJSON(w, status, out)
}

// events streams a session's events as Server-Sent Events. A client resumes
// with Last-Event-ID or ?after=<seq>.
func (h *httpAPI) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, domain.Errorf(domain.ErrInternal, "streaming unsupported"))
		return
	}
	after := int64(0)
	for _, v := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("after")} {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > after {
			after = n
		}
	}
	replay, ch, cancel, err := h.api.Hub.Subscribe(clientOf(r.Context()), r.PathValue("id"), after)
	if err != nil {
		writeErr(w, HTTPStatus(domain.CodeOf(err)), err)
		return
	}
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	send := func(e domain.Event) {
		e.Raw = nil
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.Seq, e.Type, b)
	}
	for _, e := range replay {
		send(e)
	}
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case e, ok := <-ch:
			if !ok {
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				fl.Flush()
				return
			}
			send(e)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// limiter is a token bucket per client.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(rate float64) *limiter {
	if rate <= 0 {
		rate = 20
	}
	return &limiter{rate: rate, buckets: map[string]*bucket{}}
}

func (l *limiter) allow(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b := l.buckets[client]
	if b == nil {
		b = &bucket{tokens: 2 * l.rate, last: now}
		l.buckets[client] = b
	}
	b.tokens = min(2*l.rate, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
