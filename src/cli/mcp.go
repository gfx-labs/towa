package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/tools/mcp"
)

// errMCPServer is a server name the mcp commands cannot act on: it is not
// in the configuration, or it is not a remote server.
var errMCPServer = errors.New("no remote MCP server of that name")

// mcpEnv is what the mcp commands take from outside the configuration. A
// test stands in for the browser.
type mcpEnv struct {
	openBrowser func(ctx context.Context, url string) error
}

// mcpCmd holds login and logout for the OAuth login of a configured remote
// MCP server.
type mcpCmd struct {
	Login  mcpLoginCmd  `cmd:"" help:"Log in to a remote MCP server from mcp.servers and store its tokens."`
	Logout mcpLogoutCmd `cmd:"" help:"Delete a remote MCP server's stored tokens."`
}

type mcpLoginCmd struct {
	Server string `arg:"" help:"The name of the server in mcp.servers."`
	Manual bool   `help:"Print the page to open and read a pasted address, for a host without a browser."`
}

type mcpLogoutCmd struct {
	Server string `arg:"" help:"The name of the server in mcp.servers."`
}

// remoteMCPServer finds the server of that name in the configuration and
// checks that it is remote. It sends nothing.
func remoteMCPServer(name string, h Host, cfg config.MCP) (mcp.Server, error) {
	lookupEnv := func(variable string) (string, bool) {
		for _, kv := range slices.Backward(h.Environ) {
			if k, v, ok := strings.Cut(kv, "="); ok && k == variable {
				return v, true
			}
		}
		return "", false
	}
	// A session's servers are not known here: only the configuration's.
	servers, err := mcp.Servers(cfg, nil, lookupEnv)
	if err != nil {
		return mcp.Server{}, err
	}
	i := slices.IndexFunc(servers, func(s mcp.Server) bool { return s.Name == name })
	if i < 0 {
		return mcp.Server{}, fmt.Errorf("%q is not in mcp.servers: %w", name, errMCPServer)
	}
	if servers[i].Transport == config.MCPTransportStdio {
		return mcp.Server{}, fmt.Errorf("%q is a %s server, which has no login: %w", name, servers[i].Transport, errMCPServer)
	}
	return servers[i], nil
}

// Run logs in to the server and stores its tokens.
func (c *mcpLoginCmd) Run(ctx context.Context, loaded *config.Loaded, h Host, env mcpEnv) error {
	server, err := remoteMCPServer(c.Server, h, loaded.Config.MCP)
	if err != nil {
		return err
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}
	err = mcp.Login(ctx, mcp.LoginOptions{
		Server:        server,
		Config:        loaded.Config.MCP.OAuth,
		HTTPClient:    httpclient.New(),
		Tokens:        store,
		Manual:        c.Manual,
		OpenURL:       env.openBrowser,
		In:            h.Stdin,
		Out:           h.Stderr,
		ClientName:    "towa",
		ClientVersion: version(),
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(h.Stdout, "MCP server %s: logged in at %s\n", server.Name, server.URL)
	return err
}

// Run deletes the server's stored login.
func (c *mcpLogoutCmd) Run(ctx context.Context, loaded *config.Loaded, h Host) error {
	server, err := remoteMCPServer(c.Server, h, loaded.Config.MCP)
	if err != nil {
		return err
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}
	// Under the lock, so that a refresh running in another process does
	// not write the login back.
	unlock, err := store.LockMCP(ctx, server.Name)
	if err != nil {
		return err
	}
	if err := errors.Join(store.DeleteMCP(server.Name), unlock()); err != nil {
		return err
	}
	// Nothing is revoked: the stored login does not hold a revocation
	// endpoint, and finding one means asking the server again.
	_, err = fmt.Fprintf(h.Stdout, "MCP server %s: the stored login is deleted (it is not revoked at the authorization server)\n", server.Name)
	return err
}
