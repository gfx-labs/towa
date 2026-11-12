package mcp

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"

	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/towa/src/config"
)

// Pool holds the connections to the servers configuration marks shared: one
// per server for the whole process, used by the manager of every session.
// A connection starts on first use and closes when no session has used it
// for the idle time, as a session's own do. Ending a session closes none of
// them. Close closes them for good.
//
// Sharing changes what a server sees. Every session's calls arrive on one
// MCP session, so a server that keeps state between calls (a working
// directory, a login, a cursor) shows one session what another did. A
// shared stdio server runs once, in the process's working directory and
// with the process's environment, not a session's.
type Pool struct {
	m *Manager
}

// PoolOptions configures a Pool.
type PoolOptions struct {
	Config config.MCP
	// Servers are the configured servers, from Servers with no session.
	// The pool takes the ones marked shared.
	Servers []Server
	// HTTPClient, Tokens, Logger, Tracer, ClientName and ClientVersion are
	// as in Options.
	HTTPClient    *http.Client
	Tokens        TokenStore
	Logger        *slog.Logger
	Tracer        trace.Tracer
	ClientName    string
	ClientVersion string
}

// NewPool returns the pool for the shared servers among opts.Servers. It
// starts nothing.
func NewPool(opts PoolOptions) (*Pool, error) {
	var shared []Server
	for _, s := range opts.Servers {
		if !s.Shared {
			continue
		}
		if s.FromSession {
			return nil, fmt.Errorf("mcp: server %q is named by a session and cannot be shared", s.Name)
		}
		shared = append(shared, s)
	}
	m, err := newManager(Options{
		Config: opts.Config, Servers: shared, HTTPClient: opts.HTTPClient, Tokens: opts.Tokens,
		Logger: opts.Logger, Tracer: opts.Tracer, ClientName: opts.ClientName, ClientVersion: opts.ClientVersion,
	}, true)
	if err != nil {
		return nil, err
	}
	return &Pool{m: m}, nil
}

// Close closes every shared connection and ends every shared stdio server.
// Calls in flight on them fail, and later ones get ErrClosed.
func (p *Pool) Close() error { return p.m.Close() }

// connection returns the pool's connection for a session's shared server.
func (p *Pool) connection(s Server) (*connection, error) {
	if p == nil {
		return nil, fmt.Errorf("mcp: server %q is shared and Options.Pool is not set", s.Name)
	}
	if s.FromSession {
		return nil, fmt.Errorf("mcp: server %q is named by a session and cannot be shared", s.Name)
	}
	c, ok := p.m.conns[s.Name]
	if !ok {
		return nil, fmt.Errorf("mcp: shared server %q is not in the pool", s.Name)
	}
	// A session whose configuration gives the server another command, URL
	// or header would otherwise be handed a connection to something else.
	if !reflect.DeepEqual(c.server, s) {
		return nil, errors.New("mcp: shared server " + s.Name + " is defined differently for this session than in the pool")
	}
	return c, nil
}
