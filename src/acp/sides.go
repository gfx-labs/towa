package acp

import (
	"context"
	"io"
	"log/slog"
)

// AgentSideConnection is the agent's end of an ACP connection. It passes the
// client's requests and notifications to an Agent, and its methods call the
// client.
type AgentSideConnection struct {
	conn  *Connection
	agent Agent
}

// NewAgentSideConnection returns the agent's end of a connection that writes
// to w and reads from r once Serve is called. A nil logger discards log
// records.
//
// Requests are handled concurrently, so agent orders a session/cancel
// against the session's prompts with Arrival. The one exception is
// initialize: it is handled in order, so it has finished before the agent
// sees any later message, including one the client sent without waiting.
func NewAgentSideConnection(agent Agent, w io.Writer, r io.Reader, logger *slog.Logger) *AgentSideConnection {
	a := &AgentSideConnection{agent: agent}
	a.conn = NewConnection(a.handle, w, r, logger)
	a.conn.inOrder = func(method string) bool { return method == AgentMethodInitialize }
	return a
}

// Serve handles the client's messages until the reader ends. See
// Connection.Serve.
func (a *AgentSideConnection) Serve(ctx context.Context) error { return a.conn.Serve(ctx) }

// Done is closed when Serve has stopped reading.
func (a *AgentSideConnection) Done() <-chan struct{} { return a.conn.Done() }

// ClientSideConnection is the client's end of an ACP connection. It passes
// the agent's requests and notifications to a Client, and its methods call
// the agent.
type ClientSideConnection struct {
	conn   *Connection
	client Client
}

// NewClientSideConnection returns the client's end of a connection that
// writes to w and reads from r once Serve is called. A nil logger discards
// log records.
func NewClientSideConnection(client Client, w io.Writer, r io.Reader, logger *slog.Logger) *ClientSideConnection {
	c := &ClientSideConnection{client: client}
	c.conn = NewConnection(c.handle, w, r, logger)
	return c
}

// Serve handles the agent's messages until the reader ends. See
// Connection.Serve.
func (c *ClientSideConnection) Serve(ctx context.Context) error { return c.conn.Serve(ctx) }

// Done is closed when Serve has stopped reading.
func (c *ClientSideConnection) Done() <-chan struct{} { return c.conn.Done() }
