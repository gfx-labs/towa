package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/mcp"
)

// errMCPServer is a server name the mcp commands cannot act on: it is not
// in the configuration, or it is not a remote server.
var errMCPServer = errors.New("no remote MCP server of that name")

// mcpEnv is what the mcp commands take from outside the configuration. A
// test stands in for the browser.
type mcpEnv struct {
	openBrowser func(ctx context.Context, url string) error
}

// mcpCommands returns the mcp command, with login and logout for the OAuth
// login of a configured remote MCP server.
func mcpCommands(h Host) []*cli.Command {
	return mcpCommandsWith(h, mcpEnv{openBrowser: auth.OpenBrowser})
}

func mcpCommandsWith(h Host, env mcpEnv) []*cli.Command {
	return []*cli.Command{{
		Name:  "mcp",
		Usage: "manage the OAuth logins of remote MCP servers",
		Commands: []*cli.Command{
			{
				Name:      "login",
				Usage:     "log in to a remote MCP server from mcp.servers and store its tokens",
				ArgsUsage: "<server>",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "manual", Usage: "print the page to open and read a pasted address, for a host without a browser"},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return mcpLogin(ctx, cmd, h, env)
				},
			},
			{
				Name:      "logout",
				Usage:     "delete a remote MCP server's stored tokens",
				ArgsUsage: "<server>",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return mcpLogout(ctx, cmd, h)
				},
			},
		},
	}}
}

// remoteMCPServer finds the server the command names in the configuration
// and checks that it is remote. It sends nothing.
func remoteMCPServer(cmd *cli.Command, h Host, cfg config.MCP) (mcp.Server, error) {
	if cmd.Args().Len() != 1 {
		return mcp.Server{}, fmt.Errorf("mcp %s takes one server name, and got %d arguments", cmd.Name, cmd.Args().Len())
	}
	name := cmd.Args().First()
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

func mcpLogin(ctx context.Context, cmd *cli.Command, h Host, env mcpEnv) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	server, err := remoteMCPServer(cmd, h, loaded.Config.MCP)
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
		Manual:        cmd.Bool("manual"),
		OpenURL:       env.openBrowser,
		In:            h.Stdin,
		Out:           h.Stderr,
		ClientName:    "acpharness",
		ClientVersion: version(),
	})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(h.Stdout, "MCP server %s: logged in at %s\n", server.Name, server.URL)
	return err
}

func mcpLogout(ctx context.Context, cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	server, err := remoteMCPServer(cmd, h, loaded.Config.MCP)
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
