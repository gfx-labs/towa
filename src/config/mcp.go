package config

import (
	"fmt"
	"net"
	"net/url"
)

// MCP configures MCP servers and the connections to them.
type MCP struct {
	// IdleCloseSeconds is how long a connection may go unused before it is
	// closed. The next use opens it again. Zero keeps connections open
	// until the session ends, which a stdio server that keeps state between
	// calls needs. Five minutes is a choice, not a measurement: long enough
	// that a server used every few turns stays up, short enough that a pool
	// of idle sessions does not hold one process per server each.
	IdleCloseSeconds int64 `name:"idle_close_seconds" default:"300" validate:"gte=0" help:"Seconds a connection may go unused before it is closed. 0 keeps connections open until the session ends."`
	// ConnectTimeoutSeconds bounds starting a server and its initialize
	// exchange. A stdio server started through npx or uvx downloads its
	// package on first run, which takes longer than a handshake.
	ConnectTimeoutSeconds int64 `name:"connect_timeout_seconds" default:"30" validate:"gt=0" help:"Seconds starting a server and its initialize exchange may take."`
	// TerminateGraceSeconds is how long a stdio server has to exit after
	// its stdin closes before its process group gets SIGTERM, and again
	// before SIGKILL. The default is the grace the MCP Go SDK gives a stdio
	// server before it signals it.
	TerminateGraceSeconds int64 `name:"terminate_grace_seconds" default:"5" validate:"gt=0" help:"Seconds a stdio server has to exit after its stdin closes before SIGTERM, and again before SIGKILL."`
	// SearchResults is the most tools one search of the mcp tool returns
	// with their schemas. The result says how many more matched.
	SearchResults int64 `name:"search_results" default:"10" validate:"gt=0" help:"Most tools one search of the mcp tool returns with their schemas."`
	// SessionServersDirect says whether a server a client names in
	// session/new has its tools put in front of the model under their own
	// names. A session's server replaces a configured server of the same
	// name whole, so this key and not that entry's direct key applies.
	SessionServersDirect bool `name:"session_servers_direct" default:"true" help:"Put the tools of a server a client names in session/new in front of the model under their own names. On by default."`
	// SessionServersTrustAnnotations says whether a tool of a session's
	// server that declares readOnlyHint is treated as read-only. The client
	// that adds a server to a session chose it for that session.
	SessionServersTrustAnnotations bool        `name:"session_servers_trust_annotations" default:"true" help:"Treat a tool of a session's server that declares readOnlyHint as read-only. On by default."`
	OAuth                          MCPOAuth    `embed:"" prefix:"oauth."`
	Servers                        []MCPServer `name:"servers" type:"table" placeholder:"YAML" help:"MCP servers: name, transport, then command or url. Written in a config file."`
}

// MCPOAuth configures the authorization code flow for remote servers.
type MCPOAuth struct {
	// CallbackAddr is the loopback address the login command listens on
	// for the redirect. Port 0 picks a free port, which works with dynamic
	// client registration. A pre-registered client needs the port it was
	// registered with.
	CallbackAddr string `name:"callback_addr" default:"127.0.0.1:0" help:"Loopback address the login command listens on for the redirect. Port 0 picks a free port."`
	// CallbackPath is the path of the redirect URL.
	CallbackPath string `name:"callback_path" default:"/callback" help:"Path of the redirect URL."`
	// LoginTimeoutSeconds is how long a login waits for the redirect. It
	// and RefreshMarginSeconds default to the values of
	// auth.login_timeout_seconds and auth.refresh_margin_seconds, which are
	// jcode's.
	LoginTimeoutSeconds int64 `name:"login_timeout_seconds" default:"300" validate:"gt=0" help:"Seconds a login waits for the redirect."`
	// RefreshMarginSeconds is how long before its expiry a token is
	// refreshed.
	RefreshMarginSeconds int64 `name:"refresh_margin_seconds" default:"60" validate:"gte=0" help:"Seconds before its expiry that a token is refreshed."`
}

// MCPServer is one server from configuration.
type MCPServer struct {
	Name string `json:"name" validate:"required"`
	// Transport is one of the MCPTransport values.
	Transport string `json:"transport" validate:"oneof=stdio streamable_http sse"`
	// Command, Args and Env start a stdio server. Env is added to the
	// harness's own environment.
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	// URL is the endpoint of a streamable_http or sse server.
	URL string `json:"url"`
	// Headers are sent with every request to the server. HeadersEnv maps a
	// header name to the environment variable that holds its value, for a
	// value that is a secret.
	Headers    map[string]string `json:"headers"`
	HeadersEnv map[string]string `json:"headers_env"`
	// Direct puts the server's tools in front of the model under their own
	// names. Unset means no.
	Direct *bool `json:"direct"`
	// TrustAnnotations treats a tool that declares readOnlyHint as
	// read-only. Without it every tool of the server counts as one that
	// executes, because a server can declare anything.
	TrustAnnotations *bool `json:"trust_annotations"`
	// Shared gives the server one connection for the whole process, used by
	// every session, in place of one per session. A shared stdio server
	// runs once, in the process's working directory and environment, and
	// whatever state it keeps is seen by every session. It does not apply
	// to a server a session names.
	Shared bool           `json:"shared"`
	OAuth  MCPServerOAuth `json:"oauth"`
}

// MCPServerOAuth names a client registered with the server's authorization
// server by hand. When ClientID is empty the client is registered
// dynamically at login.
type MCPServerOAuth struct {
	ClientID string `json:"client_id"`
	// ClientSecretEnv names the environment variable that holds the
	// client's secret. It is empty for a public client.
	ClientSecretEnv string `json:"client_secret_env"`
}

// Transports of an MCP server.
const (
	MCPTransportStdio          = "stdio"
	MCPTransportStreamableHTTP = "streamable_http"
	MCPTransportSSE            = "sse"
)

func (m MCP) validate() error {
	if err := validateFields("mcp", m); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(m.OAuth.CallbackAddr)
	if err != nil {
		return fmt.Errorf("mcp.oauth.callback_addr: %w", err)
	}
	// An authorization server accepts a plain http redirect only to a
	// loopback address.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("mcp.oauth.callback_addr: host %q is not a loopback IP address", host)
	}
	if len(m.OAuth.CallbackPath) == 0 || m.OAuth.CallbackPath[0] != '/' {
		return fmt.Errorf("mcp.oauth.callback_path: %q does not start with /", m.OAuth.CallbackPath)
	}
	seen := map[string]bool{}
	for i, s := range m.Servers {
		at := fmt.Sprintf("mcp.servers[%d]", i)
		if seen[s.Name] {
			return fmt.Errorf("%s.name: %q is used by more than one server", at, s.Name)
		}
		seen[s.Name] = true
		if err := s.validate(at); err != nil {
			return err
		}
	}
	return nil
}

func (s MCPServer) validate(at string) error {
	if err := validateFields(at, s); err != nil {
		return err
	}
	remote := s.URL != "" || len(s.Headers) > 0 || len(s.HeadersEnv) > 0 || s.OAuth != (MCPServerOAuth{})
	local := s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0
	switch s.Transport {
	case MCPTransportStdio:
		switch {
		case s.Command == "":
			return fmt.Errorf("%s.command is empty for a %s server", at, s.Transport)
		case remote:
			return fmt.Errorf("%s: url, headers, headers_env and oauth do not apply to a %s server", at, s.Transport)
		}
	case MCPTransportStreamableHTTP, MCPTransportSSE:
		if local {
			return fmt.Errorf("%s: command, args and env do not apply to a %s server", at, s.Transport)
		}
		u, err := url.Parse(s.URL)
		if err != nil {
			return fmt.Errorf("%s.url: cannot parse URL", at)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("%s.url: must be an http or https URL with a host", at)
		}
		for name := range s.HeadersEnv {
			if _, both := s.Headers[name]; both {
				return fmt.Errorf("%s: header %q is in both headers and headers_env", at, name)
			}
		}
		if s.OAuth.ClientSecretEnv != "" && s.OAuth.ClientID == "" {
			return fmt.Errorf("%s.oauth.client_secret_env is set without client_id", at)
		}
	}
	return nil
}
