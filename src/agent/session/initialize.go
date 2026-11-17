package session

import (
	"context"
	"slices"

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
		capabilities.McpCapabilities = acp.McpCapabilities{Http: mcp.Http, Sse: mcp.Sse, Acp: mcp.Acp}
	}
	return acp.InitializeResponse{
		// The only version there is to offer: the client's own when it asked
		// for it, and otherwise the latest this agent supports.
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentInfo:         &acp.Implementation{Name: "towa", Version: a.version},
		AgentCapabilities: capabilities,
		AuthMethods:       a.authMethods(p.ClientCapabilities),
	}, nil
}

// authMethods lists the terminal logins for a client that can run them. A
// client says so with auth.terminal or, as clients did before that field,
// with "terminal-auth" in _meta.
func (a *Agent) authMethods(client acp.ClientCapabilities) []acp.AuthMethod {
	if _, legacy := client.Meta["terminal-auth"]; !client.Auth.Terminal && !legacy {
		return nil
	}
	methods := make([]acp.AuthMethod, len(a.supports.TerminalAuth))
	for i := range a.supports.TerminalAuth {
		methods[i] = acp.AuthMethod{Terminal: &a.supports.TerminalAuth[i].Method}
	}
	return methods
}

// Authenticate checks that a terminal method's login was run. The client
// runs the login itself, and some clients then call authenticate with its id.
func (a *Agent) Authenticate(_ context.Context, p acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	i := slices.IndexFunc(a.supports.TerminalAuth, func(l TerminalLogin) bool { return l.Method.Id == p.MethodId })
	if i < 0 {
		return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"error": "unknown authentication method", "methodId": p.MethodId})
	}
	account := a.supports.TerminalAuth[i].Account
	ok, err := a.supports.LoggedIn(account)
	if err != nil {
		return acp.AuthenticateResponse{}, acp.NewInternalError(map[string]any{"error": err.Error()})
	}
	if !ok {
		return acp.AuthenticateResponse{}, acp.NewAuthRequired(map[string]any{"error": "account " + account + " has no stored login: run the method's login in a terminal first", "methodId": p.MethodId})
	}
	return acp.AuthenticateResponse{}, nil
}

// Logout is not advertised.
func (a *Agent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
