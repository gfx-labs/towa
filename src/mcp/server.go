package mcp

import (
	"fmt"
	"maps"
	"net/url"
	"slices"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
)

// Server is one MCP server a session can use, resolved from configuration
// or from the session's mcpServers.
type Server struct {
	Name string
	// Transport is config.MCPTransportStdio, MCPTransportStreamableHTTP or
	// MCPTransportSSE.
	Transport string
	// Command and Args start a stdio server. Env holds "NAME=value" entries
	// added to the harness's own environment.
	Command string
	Args    []string
	Env     []string
	// URL is the endpoint of a remote server. Headers go on every request
	// sent to the URL's host.
	URL     string
	Headers map[string]string
	// Direct puts the server's tools in front of the model under their own
	// names. Otherwise they are reached through the mcp tool.
	Direct bool
	// TrustAnnotations treats a tool that declares readOnlyHint as
	// read-only.
	TrustAnnotations bool
	// FromSession says the session named the server, not configuration.
	FromSession bool
	// OAuthClientID and OAuthClientSecret name a client registered by hand.
	// When the id is empty a login registers a client dynamically.
	OAuthClientID     string
	OAuthClientSecret string
}

// Servers merges the servers of the configuration with the ones a session
// names. A session's server replaces a configured one with the same name,
// whole, and keeps its place in the order. lookupEnv reads the variables
// that headers_env and client_secret_env name: pass os.LookupEnv.
func Servers(cfg config.MCP, session []acp.McpServer, lookupEnv func(string) (string, bool)) ([]Server, error) {
	servers := make([]Server, 0, len(cfg.Servers)+len(session))
	index := map[string]int{}
	for i, c := range cfg.Servers {
		s, err := fromConfig(c, lookupEnv)
		if err != nil {
			return nil, fmt.Errorf("mcp.servers[%d] (%s): %w", i, c.Name, err)
		}
		index[s.Name] = len(servers)
		servers = append(servers, s)
	}
	fromSession := map[string]bool{}
	for i, a := range session {
		s, err := fromACP(a)
		if err != nil {
			return nil, fmt.Errorf("mcpServers[%d]: %w", i, err)
		}
		if fromSession[s.Name] {
			return nil, fmt.Errorf("mcpServers[%d]: name %q is used by more than one server", i, s.Name)
		}
		fromSession[s.Name] = true
		s.FromSession = true
		s.Direct = cfg.SessionServersDirect
		s.TrustAnnotations = cfg.SessionServersTrustAnnotations
		if at, ok := index[s.Name]; ok {
			servers[at] = s
			continue
		}
		index[s.Name] = len(servers)
		servers = append(servers, s)
	}
	return servers, nil
}

func fromConfig(c config.MCPServer, lookupEnv func(string) (string, bool)) (Server, error) {
	s := Server{
		Name:          c.Name,
		Transport:     c.Transport,
		Command:       c.Command,
		Args:          slices.Clone(c.Args),
		URL:           c.URL,
		Headers:       maps.Clone(c.Headers),
		OAuthClientID: c.OAuth.ClientID,
	}
	if c.Direct != nil {
		s.Direct = *c.Direct
	}
	if c.TrustAnnotations != nil {
		s.TrustAnnotations = *c.TrustAnnotations
	}
	// Sorted so that the child's environment does not change between runs.
	for _, name := range slices.Sorted(maps.Keys(c.Env)) {
		s.Env = append(s.Env, name+"="+c.Env[name])
	}
	for header, variable := range c.HeadersEnv {
		value, ok := lookupEnv(variable)
		if !ok {
			return Server{}, fmt.Errorf("headers_env: %s is not set, for header %q", variable, header)
		}
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		s.Headers[header] = value
	}
	if variable := c.OAuth.ClientSecretEnv; variable != "" {
		secret, ok := lookupEnv(variable)
		if !ok {
			return Server{}, fmt.Errorf("oauth.client_secret_env: %s is not set", variable)
		}
		s.OAuthClientSecret = secret
	}
	return s, nil
}

func fromACP(a acp.McpServer) (Server, error) {
	var s Server
	switch {
	case a.Stdio != nil:
		s = Server{Name: a.Stdio.Name, Transport: config.MCPTransportStdio, Command: a.Stdio.Command, Args: slices.Clone(a.Stdio.Args)}
		for _, e := range a.Stdio.Env {
			s.Env = append(s.Env, e.Name+"="+e.Value)
		}
		if s.Command == "" {
			return Server{}, fmt.Errorf("stdio server %q has no command", s.Name)
		}
	case a.Http != nil:
		s = Server{Name: a.Http.Name, Transport: config.MCPTransportStreamableHTTP, URL: a.Http.Url}
		s.Headers = acpHeaders(a.Http.Headers)
	case a.Sse != nil:
		s = Server{Name: a.Sse.Name, Transport: config.MCPTransportSSE, URL: a.Sse.Url}
		s.Headers = acpHeaders(a.Sse.Headers)
	case a.Acp != nil:
		return Server{}, fmt.Errorf("server %q uses the acp transport, which is not supported", a.Acp.Name)
	default:
		return Server{}, fmt.Errorf("server has no transport")
	}
	if s.Name == "" {
		return Server{}, fmt.Errorf("server has no name")
	}
	if s.Transport != config.MCPTransportStdio {
		u, err := url.Parse(s.URL)
		if err != nil {
			return Server{}, fmt.Errorf("server %q: %w", s.Name, err)
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Server{}, fmt.Errorf("server %q: %q is not an http or https URL", s.Name, s.URL)
		}
	}
	return s, nil
}

func acpHeaders(headers []acp.HttpHeader) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(headers))
	for _, h := range headers {
		out[h.Name] = h.Value
	}
	return out
}
