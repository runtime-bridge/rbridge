package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"

	"github.com/runtime-bridge/rbridge/internal/buildinfo"
	"github.com/runtime-bridge/rbridge/internal/domain"
)

// IPCProtocolVersion is the version of the IPC protocol in specs/ipc-protocol.md.
const IPCProtocolVersion = 1

type rpcMsg struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  any              `json:"result,omitempty"`
	Error   *rpcError        `json:"error,omitempty"`
}

type rpcError struct {
	Code    int       `json:"code"`
	Message string    `json:"message"`
	Data    ErrorBody `json:"data"`
}

// ServeIPC accepts connections on l until it is closed.
func ServeIPC(ctx context.Context, l net.Listener, api *API) error {
	methods := api.Methods()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		api.Connections.Add(1)
		go func() {
			defer wg.Done()
			defer api.Connections.Add(-1)
			(&ipcConn{conn: c, api: api, methods: methods}).serve(ctx)
		}()
	}
}

type ipcConn struct {
	conn    net.Conn
	api     *API
	methods map[string]Method

	writeMu sync.Mutex
	mu      sync.Mutex
	owner   string
	subs    map[string]func()
}

func (c *ipcConn) write(m rpcMsg) {
	m.JSONRPC = "2.0"
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, _ = c.conn.Write(append(b, '\n'))
}

func (c *ipcConn) fail(id *json.RawMessage, code int, err error) {
	c.write(rpcMsg{ID: id, Error: &rpcError{Code: code, Message: err.Error(), Data: errorBody(err)}})
}

func (c *ipcConn) serve(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		cancel()
		c.mu.Lock()
		for _, stop := range c.subs {
			stop()
		}
		c.mu.Unlock()
		c.conn.Close()
	}()
	go func() {
		<-ctx.Done()
		c.conn.Close()
	}()
	c.subs = map[string]func(){}
	sc := bufio.NewScanner(c.conn)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	var wg sync.WaitGroup
	// When the client goes, its requests are cancelled before they are waited
	// for: an open waiting for a free slot would otherwise wait for ever, and
	// keep this connection's subscriptions, so its idle sessions are never
	// closed and keep the slots it is waiting for.
	defer func() {
		cancel()
		wg.Wait()
	}()
	for sc.Scan() {
		var m rpcMsg
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			c.fail(nil, -32700, domain.Errorf(domain.ErrInvalidInput, "parse error"))
			continue
		}
		if m.ID == nil {
			continue // notifications from clients are not used
		}
		if m.Method == "initialize" {
			c.initialize(m)
			continue
		}
		c.mu.Lock()
		owner := c.owner
		c.mu.Unlock()
		if owner == "" {
			c.fail(m.ID, -32002, domain.Errorf(domain.ErrInvalidInput, "call initialize first"))
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.handle(ctx, owner, m)
		}()
	}
}

func (c *ipcConn) initialize(m rpcMsg) {
	var p struct {
		Client          string `json:"client"`
		ProtocolVersion int    `json:"protocol_version"`
	}
	_ = json.Unmarshal(m.Params, &p)
	if p.Client == "" {
		p.Client = "default"
	}
	if !ValidClient(p.Client) {
		c.fail(m.ID, -32602, domain.Errorf(domain.ErrInvalidInput, "invalid client name"))
		return
	}
	if p.ProtocolVersion != 0 && p.ProtocolVersion != IPCProtocolVersion {
		c.fail(m.ID, -32602, domain.Errorf(domain.ErrInvalidInput, "unsupported protocol version %d", p.ProtocolVersion))
		return
	}
	c.mu.Lock()
	c.owner = p.Client
	c.mu.Unlock()
	c.write(rpcMsg{ID: m.ID, Result: map[string]any{"protocol_version": IPCProtocolVersion, "version": buildinfo.Get().Version}})
}

func (c *ipcConn) handle(ctx context.Context, owner string, m rpcMsg) {
	switch m.Method {
	case "sessions.subscribe":
		c.subscribe(owner, m)
		return
	case "sessions.unsubscribe":
		var p struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		if stop := c.subs[p.ID]; stop != nil {
			stop()
			delete(c.subs, p.ID)
		}
		c.mu.Unlock()
		c.write(rpcMsg{ID: m.ID, Result: map[string]bool{"unsubscribed": true}})
		return
	}
	fn := c.methods[m.Method]
	if fn == nil {
		c.fail(m.ID, -32601, domain.Errorf(domain.ErrInvalidInput, "unknown method %q", m.Method))
		return
	}
	out, err := fn(ctx, owner, m.Params)
	if err != nil {
		c.fail(m.ID, -32000, err)
		return
	}
	c.write(rpcMsg{ID: m.ID, Result: out})
}

// subscribe streams a session's events to this connection as session.event
// notifications until the session closes or the client unsubscribes.
func (c *ipcConn) subscribe(owner string, m rpcMsg) {
	var p struct {
		ID    string `json:"id"`
		After int64  `json:"after"`
	}
	_ = json.Unmarshal(m.Params, &p)
	replay, ch, stop, err := c.api.Hub.Subscribe(owner, p.ID, p.After)
	if err != nil {
		c.fail(m.ID, -32000, err)
		return
	}
	c.mu.Lock()
	if old := c.subs[p.ID]; old != nil {
		old()
	}
	c.subs[p.ID] = stop
	c.mu.Unlock()
	c.write(rpcMsg{ID: m.ID, Result: map[string]bool{"subscribed": true}})
	notify := func(e domain.Event) {
		e.Raw = nil
		b, _ := json.Marshal(map[string]any{"event": e})
		c.write(rpcMsg{Method: "session.event", Params: b})
	}
	for _, e := range replay {
		notify(e)
	}
	go func() {
		for e := range ch {
			notify(e)
		}
		b, _ := json.Marshal(map[string]string{"id": p.ID})
		c.write(rpcMsg{Method: "session.end", Params: b})
	}()
}
