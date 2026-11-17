package session

import acp "github.com/gfx-labs/go-acp"

// Gate holds what the client of one connection advertised in initialize and
// decides which session updates it is sent.
//
// An update type that the protocol puts behind a client capability is sent
// only when the client advertised it. A client that predates the type does
// not reject it: acp-go-sdk v0.13.5 decodes a notice as a session info
// update and a compaction summary chunk as a user message chunk, without an
// error.
type Gate struct {
	client acp.ClientCapabilities
}

// NewGate returns the gate for a client that advertised c.
func NewGate(c acp.ClientCapabilities) Gate { return Gate{client: c} }

// Client returns what the client advertised.
func (g Gate) Client() acp.ClientCapabilities { return g.client }

// Sendable reports whether the client advertised the type of update. Types
// behind no capability are always sendable.
func (g Gate) Sendable(update acp.SessionUpdate) bool {
	session := g.client.Session
	switch {
	case update.Notice != nil:
		return session != nil && session.Notices != nil
	case update.CompactionUpdate != nil, update.CompactionSummaryChunk != nil:
		return session != nil && session.Compaction != nil
	case update.PlanUpdate != nil, update.PlanRemoved != nil:
		return g.client.Plan != nil
	case update.SubagentUpdate != nil, update.SessionMessage != nil, update.SessionMessageChunk != nil:
		return g.client.Subagents != nil
	}
	return true
}

// BooleanOptions reports whether the client takes config options of type
// boolean. Without it the agent lists and accepts select options only.
func (g Gate) BooleanOptions() bool {
	session := g.client.Session
	return session != nil && session.ConfigOptions != nil && session.ConfigOptions.Boolean != nil
}
