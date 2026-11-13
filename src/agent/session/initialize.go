package session

import (
	"context"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
)

// Initialize records what the client supports and answers with the protocol
// version and the agent's capabilities.
func (a *Agent) Initialize(_ context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	a.liveMu.Lock()
	ended := a.ended || a.ending
	a.liveMu.Unlock()
	if ended {
		return acp.InitializeResponse{}, acp.NewInvalidRequest(map[string]any{"error": "connection or host has ended"})
	}
	a.mu.Lock()
	a.initialized = true
	a.gate = NewGate(p.ClientCapabilities)
	a.mu.Unlock()

	sessions := acp.SessionCapabilities{
		Close:   &acp.SessionCloseCapabilities{},
		Options: &acp.SessionOptionsCapabilities{},
		Status:  &acp.SessionStatusCapabilities{},
		Title:   &acp.SessionTitleCapabilities{},
	}
	if a.supports.AdditionalDirectories {
		sessions.AdditionalDirectories = &acp.SessionAdditionalDirectoriesCapabilities{}
	}
	if a.supports.SystemPrompt {
		sessions.SystemPrompt = agentctx.Capabilities()
	}
	capabilities := acp.AgentCapabilities{SessionCapabilities: sessions, PromptCapabilities: a.supports.Prompt}
	if a.store != nil {
		capabilities.LoadSession = true
		capabilities.SessionCapabilities.Resume = &acp.SessionResumeCapabilities{}
		capabilities.SessionCapabilities.List = &acp.SessionListCapabilities{}
		capabilities.SessionCapabilities.Delete = &acp.SessionDeleteCapabilities{}
		capabilities.SessionCapabilities.Fork = &acp.SessionForkCapabilities{}
		capabilities.SessionCapabilities.Archive = &acp.SessionArchiveCapabilities{}
	}
	if mcp := a.supports.MCP; mcp != nil {
		// The ACP transport for MCP is not implemented, whatever the
		// wiring says.
		capabilities.McpCapabilities = acp.McpCapabilities{Http: mcp.Http, Sse: mcp.Sse}
	}
	return acp.InitializeResponse{
		// The only version there is to offer: the client's own when it asked
		// for it, and otherwise the latest this agent supports.
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentInfo:         &acp.Implementation{Name: "towa", Version: a.version},
		AgentCapabilities: capabilities,
	}, nil
}

// Authenticate rejects every method: the agent advertises none.
func (a *Agent) Authenticate(_ context.Context, p acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown authentication method", "methodId": p.MethodId})
}

// Logout is not advertised.
func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
