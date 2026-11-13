// Package session implements the ACP agent: it owns the sessions of one
// connection and runs their prompt turns.
package session

import (
	"sync"

	acp "github.com/gfx-labs/go-acp"
)

// Agent is the ACP agent for one connection. It advertises only what it
// implements: a method behind a capability it does not advertise answers
// "Method not found".
//
// What a process has once (the store, the registry, the table of live
// sessions) is in the Host the agent was made from. The agent itself holds
// what belongs to its connection: the client and what the client
// advertised.
type Agent struct {
	*shared
	client Client
	// ended is set once the connection has ended. The host's table lock
	// guards it.
	ended bool

	mu          sync.Mutex
	initialized bool
	gate        Gate
}

var _ acp.Agent = (*Agent)(nil)

// SetClient sets the client the agent sends session updates to. The agent is
// needed to make the connection and the connection is the client, so this
// is a separate step.
func (a *Agent) SetClient(c Client) { a.client = c }

// Gate returns what the client advertised in initialize. Before initialize
// it is the gate of a client that advertised nothing.
func (a *Agent) Gate() Gate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.gate
}

// ready reports a request that came before initialize.
func (a *Agent) ready() error {
	a.liveMu.Lock()
	ended := a.ended || a.ending
	a.liveMu.Unlock()
	if ended {
		return acp.NewInvalidRequest(map[string]any{"error": "connection or host has ended"})
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.initialized {
		return acp.NewInvalidRequest(map[string]any{"error": "initialize must be called first"})
	}
	return nil
}

// session returns a session that is live on this connection, or the error
// for an unknown one.
func (a *Agent) session(id acp.SessionId) (*session, error) {
	if err := a.ready(); err != nil {
		return nil, err
	}
	// A session that is live on another connection is unknown to this one
	// until it loads or resumes it.
	s, owner := a.lookup(id)
	if s == nil || owner != a {
		return nil, unknownSession(id)
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return nil, unknownSession(id)
	}
	return s, nil
}

func unknownSession(id acp.SessionId) *acp.RequestError {
	return acp.NewResourceNotFound(map[string]any{"sessionId": id})
}
