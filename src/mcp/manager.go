// Package mcp connects a session to MCP servers and turns their tools into
// tools.Tool values: one mcp tool that lists, searches and calls the tools
// of every server behind it, and one tool per tool of a server marked
// direct.
//
// The protocol is the MCP Go SDK's. This package adds what a harness needs
// around it: connections that start on first use and close when idle, a
// stdio server that dies with its process group, HTTP clients that come
// from src/httpclient, and OAuth tokens kept in the auth store.
package mcp

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/gfx-labs/acpharness/src/config"
)

// ErrClosed is returned for a use of a Manager after Close.
var ErrClosed = errors.New("mcp: manager is closed")

// UnknownServerError names a server the manager does not have.
type UnknownServerError struct{ Server string }

func (e *UnknownServerError) Error() string {
	return fmt.Sprintf("mcp: no server named %q", e.Server)
}

// CallError is a tool call that was sent and did not come back with a
// result. Unless Err is a *jsonrpc.Error, which is the server's refusal,
// nothing says whether the call took effect.
type CallError struct {
	Server string
	Tool   string
	Err    error
}

func (e *CallError) Error() string {
	return fmt.Sprintf("mcp: server %q: call %q: %v", e.Server, e.Tool, e.Err)
}

func (e *CallError) Unwrap() error { return e.Err }

// Options configures a Manager.
type Options struct {
	Config config.MCP
	// Servers are the servers of one session, from Servers.
	Servers []Server
	// HTTPClient is the client from src/httpclient that remote servers are
	// reached through. It is required when a server is remote. The SDK
	// would use http.DefaultClient without it, so there is no default here.
	HTTPClient *http.Client
	// Dir is the directory a stdio server is started in: the session's
	// working directory. Empty means the harness's own.
	Dir string
	// Tokens holds OAuth logins of remote servers. It may be nil, and then
	// no server is sent a token.
	Tokens TokenStore
	// Logger gets connection events and, at debug level, what a stdio
	// server writes to its stderr. It may be nil.
	Logger *slog.Logger
	// ClientName and ClientVersion are what a server is told about the
	// harness in initialize.
	ClientName    string
	ClientVersion string
}

// Manager holds the connections of one session to its MCP servers. A
// connection starts when a server is first used and closes after it has
// been idle for the configured time. Its methods may be called from several
// goroutines.
type Manager struct {
	servers []Server
	conns   map[string]*connection
	client  *http.Client
	tokens  TokenStore
	logger  *slog.Logger
	impl    *sdk.Implementation
	results int
	dir     string

	// These are fields so that a test can use times shorter than a second,
	// the unit of the config keys.
	idleClose      time.Duration
	connectTimeout time.Duration
	terminateGrace time.Duration
	refreshMargin  time.Duration

	mu     sync.Mutex
	closed bool
}

// NewManager checks the servers and returns a manager for them. It starts
// nothing.
func NewManager(opts Options) (*Manager, error) {
	if opts.ClientName == "" || opts.ClientVersion == "" {
		return nil, errors.New("mcp: ClientName and ClientVersion are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	m := &Manager{
		servers:        opts.Servers,
		conns:          make(map[string]*connection, len(opts.Servers)),
		client:         opts.HTTPClient,
		tokens:         opts.Tokens,
		logger:         logger,
		impl:           &sdk.Implementation{Name: opts.ClientName, Version: opts.ClientVersion},
		results:        int(opts.Config.SearchResults),
		dir:            opts.Dir,
		refreshMargin:  time.Duration(opts.Config.OAuth.RefreshMarginSeconds) * time.Second,
		idleClose:      time.Duration(opts.Config.IdleCloseSeconds) * time.Second,
		connectTimeout: time.Duration(opts.Config.ConnectTimeoutSeconds) * time.Second,
		terminateGrace: time.Duration(opts.Config.TerminateGraceSeconds) * time.Second,
	}
	for _, s := range opts.Servers {
		if _, dup := m.conns[s.Name]; dup {
			return nil, fmt.Errorf("mcp: server name %q is used more than once", s.Name)
		}
		switch s.Transport {
		case config.MCPTransportStdio:
		case config.MCPTransportStreamableHTTP, config.MCPTransportSSE:
			if opts.HTTPClient == nil {
				return nil, fmt.Errorf("mcp: server %q is remote and Options.HTTPClient is not set", s.Name)
			}
		default:
			return nil, fmt.Errorf("mcp: server %q has unknown transport %q", s.Name, s.Transport)
		}
		m.conns[s.Name] = &connection{m: m, server: s, gate: make(chan struct{}, 1)}
	}
	return m, nil
}

// Servers returns the manager's servers in order.
func (m *Manager) Servers() []Server { return m.servers }

// Connected reports whether the server has a live connection now.
func (m *Manager) Connected(server string) bool {
	c, ok := m.conns[server]
	if !ok {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live != nil && !c.live.isDead()
}

// ListTools returns the server's tools, connecting to it if needed. The
// list is kept for as long as the connection lives and the server does not
// announce a change. The caller must not change the returned tools.
func (m *Manager) ListTools(ctx context.Context, server string) ([]*sdk.Tool, error) {
	c, l, err := m.acquire(ctx, server)
	if err != nil {
		return nil, err
	}
	defer c.release()
	list, err := l.listTools(ctx)
	return list, l.http.loginRequired(server, err)
}

// CallTool calls one tool, connecting to its server if needed. arguments is
// a JSON object, or empty for a tool that takes none. A failure before the
// call was sent is a connect error. One after it is a *CallError, and the
// call is never sent again.
func (m *Manager) CallTool(ctx context.Context, server, tool string, arguments jsontext.Value) (*sdk.CallToolResult, error) {
	c, l, err := m.acquire(ctx, server)
	if err != nil {
		return nil, err
	}
	defer c.release()
	params := &sdk.CallToolParams{Name: tool}
	if len(arguments) > 0 {
		// The SDK encodes a jsontext.Value as it is.
		params.Arguments = arguments
	}
	res, err := l.session.CallTool(ctx, params)
	if err != nil {
		return nil, &CallError{Server: server, Tool: tool, Err: l.http.loginRequired(server, err)}
	}
	return res, nil
}

// Close closes every connection and ends every stdio server. Calls in
// flight fail. It is safe to call more than once.
func (m *Manager) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range m.conns {
		wg.Go(func() {
			c.gate <- struct{}{}
			defer func() { <-c.gate }()
			c.mu.Lock()
			l := c.live
			c.live = nil
			c.generation++
			c.mu.Unlock()
			if l != nil {
				l.close()
			}
		})
	}
	wg.Wait()
	return nil
}

func (m *Manager) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// connection is the manager's slot for one server.
type connection struct {
	m      *Manager
	server Server
	// gate is held while the connection is being opened or closed. It is a
	// channel so that waiting for it ends with the caller's context.
	gate chan struct{}

	mu    sync.Mutex
	live  *live
	inUse int
	// generation changes on every acquire and release, so that an idle
	// timer armed before the last use does nothing.
	generation uint64
}

// acquire returns the server's live connection, opening one if there is
// none. The caller calls release when its request is done.
func (m *Manager) acquire(ctx context.Context, server string) (*connection, *live, error) {
	c, ok := m.conns[server]
	if !ok {
		return nil, nil, &UnknownServerError{Server: server}
	}
	select {
	case c.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	defer func() { <-c.gate }()
	if m.isClosed() {
		return nil, nil, ErrClosed
	}
	c.mu.Lock()
	l := c.live
	if l != nil && !l.isDead() {
		c.inUse++
		c.generation++
		c.mu.Unlock()
		return c, l, nil
	}
	c.live = nil
	c.mu.Unlock()
	if l != nil {
		// The server went away on its own. Release what is left of it.
		l.close()
	}
	l, err := m.connect(ctx, c.server)
	if err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	c.live = l
	c.inUse++
	c.generation++
	c.mu.Unlock()
	return c, l, nil
}

func (c *connection) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inUse--
	c.generation++
	if c.inUse > 0 || c.m.idleClose <= 0 {
		return
	}
	generation := c.generation
	time.AfterFunc(c.m.idleClose, func() { c.closeIdle(generation) })
}

// closeIdle closes the connection if nothing has used it since the timer
// was armed.
func (c *connection) closeIdle(generation uint64) {
	c.gate <- struct{}{}
	defer func() { <-c.gate }()
	c.mu.Lock()
	if c.generation != generation || c.live == nil {
		c.mu.Unlock()
		return
	}
	l := c.live
	c.live = nil
	c.mu.Unlock()
	c.m.logger.Debug("mcp: closing idle connection", "server", c.server.Name)
	l.close()
}

// live is one open connection.
type live struct {
	server  string
	session *sdk.ClientSession
	// cancel ends the context the connection was opened with, which an SSE
	// connection's event stream lives on.
	cancel context.CancelFunc
	// proc is the server's process, for a stdio server.
	proc *process
	// http is the transport of a remote server's requests. It is nil for a
	// stdio server.
	http *serverTransport
	// dead is closed when the session has ended, whoever ended it.
	dead chan struct{}

	mu sync.Mutex
	// tools is nil until the first listing. toolsVersion changes when the
	// server says its tools changed, so that a listing that was under way
	// is not kept.
	tools        []*sdk.Tool
	toolsListed  bool
	toolsVersion uint64
}

func (l *live) isDead() bool {
	select {
	case <-l.dead:
		return true
	default:
		return false
	}
}

func (l *live) close() {
	// For a stdio server this closes its stdin and waits for it to exit.
	_ = l.session.Close()
	<-l.dead
	l.cancel()
	if l.proc != nil {
		l.proc.stop()
	}
}

func (l *live) toolsChanged() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tools, l.toolsListed = nil, false
	l.toolsVersion++
}

func (l *live) listTools(ctx context.Context) ([]*sdk.Tool, error) {
	l.mu.Lock()
	if l.toolsListed {
		defer l.mu.Unlock()
		return l.tools, nil
	}
	version := l.toolsVersion
	l.mu.Unlock()
	var list []*sdk.Tool
	// Tools follows the server's pagination to the end.
	for tool, err := range l.session.Tools(ctx, nil) {
		if err != nil {
			return nil, fmt.Errorf("mcp: server %q: list tools: %w", l.server, err)
		}
		list = append(list, tool)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.toolsVersion == version {
		l.tools, l.toolsListed = list, true
	}
	return list, nil
}

// connect starts the server or connects to it, and runs initialize.
func (m *Manager) connect(ctx context.Context, s Server) (*live, error) {
	// An SSE connection reads its events on the request made with the
	// context given to Connect, so that context has to live as long as the
	// connection. The timeout and the caller's cancellation are applied by
	// hand and taken off again once initialize has answered.
	connCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var timedOut bool
	timer := time.AfterFunc(m.connectTimeout, func() {
		timedOut = true
		cancel()
	})
	stopFollowing := context.AfterFunc(ctx, cancel)

	l := &live{server: s.Name, cancel: cancel, dead: make(chan struct{})}
	client := sdk.NewClient(m.impl, &sdk.ClientOptions{
		ToolListChangedHandler: func(context.Context, *sdk.ToolListChangedRequest) { l.toolsChanged() },
	})
	var transport sdk.Transport
	var stdio *stdioTransport
	switch s.Transport {
	case config.MCPTransportStdio:
		stdio = &stdioTransport{server: s, dir: m.dir, grace: m.terminateGrace, logger: m.logger}
		transport = stdio
	case config.MCPTransportStreamableHTTP, config.MCPTransportSSE:
		httpClient, serverTransport, err := m.httpClientFor(s)
		if err != nil {
			cancel()
			return nil, err
		}
		l.http = serverTransport
		if s.Transport == config.MCPTransportSSE {
			transport = &sdk.SSEClientTransport{Endpoint: s.URL, HTTPClient: httpClient}
		} else {
			transport = &sdk.StreamableClientTransport{Endpoint: s.URL, HTTPClient: httpClient}
		}
	}
	session, err := client.Connect(connCtx, transport, nil)
	stopped := timer.Stop()
	stopFollowing()
	if err != nil {
		cancel()
		if stdio != nil && stdio.proc != nil {
			stdio.proc.stop()
		}
		if !stopped && timedOut {
			return nil, fmt.Errorf("mcp: server %q did not answer initialize within mcp.connect_timeout_seconds (%s): %w",
				s.Name, m.connectTimeout, err)
		}
		return nil, l.http.loginRequired(s.Name, fmt.Errorf("mcp: server %q: connect: %w", s.Name, err))
	}
	l.session = session
	if stdio != nil {
		l.proc = stdio.proc
	}
	go func() {
		// Wait returns when the session ends: closed here, or the server
		// exited or hung up.
		_ = session.Wait()
		close(l.dead)
	}()
	m.logger.Debug("mcp: connected", "server", s.Name, "transport", s.Transport)
	return l, nil
}
