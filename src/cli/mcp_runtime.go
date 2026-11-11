package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/mcp"
	"github.com/gfx-labs/acpharness/src/tools"
)

// mcpRuntimeOptions holds process dependencies. Session setup takes a copy
// with the session's tracer, leaving the pool's process tracer unchanged.
type mcpRuntimeOptions struct {
	Config     config.MCP
	LookupEnv  func(string) (string, bool)
	HTTPClient *http.Client
	Tokens     *auth.Store
	Logger     *slog.Logger
	Tracer     trace.Tracer
	Version    string
}

func (o mcpRuntimeOptions) validate() error {
	switch {
	case o.LookupEnv == nil:
		return errors.New("MCP runtime: environment lookup is required")
	case o.HTTPClient == nil:
		return errors.New("MCP runtime: shared HTTP client is required")
	case o.Tokens == nil:
		return errors.New("MCP runtime: auth store is required")
	default:
		return nil
	}
}

// newMCPPool starts nothing. Its caller closes the pool only after all
// session managers and their turns have stopped.
func newMCPPool(o mcpRuntimeOptions) (*mcp.Pool, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	servers, err := mcp.Servers(o.Config, nil, o.LookupEnv)
	if err != nil {
		return nil, err
	}
	return mcp.NewPool(mcp.PoolOptions{
		Config: o.Config, Servers: servers, HTTPClient: o.HTTPClient,
		Tokens: o.Tokens, Logger: o.Logger, Tracer: o.Tracer,
		ClientName: "acpharness", ClientVersion: o.Version,
	})
}

// newSessionMCP discovers direct tools once during session creation or
// load. Its caller keeps that slice across model switches and closes the
// manager after the session's turns stop. Setup errors go back to the ACP
// request, not to a later prompt turn.
func newSessionMCP(ctx context.Context, o mcpRuntimeOptions, pool *mcp.Pool, turn loop.Turn, taken []string) (*mcp.Manager, []tools.Tool, error) {
	if err := o.validate(); err != nil {
		return nil, nil, err
	}
	servers, err := mcp.Servers(o.Config, turn.McpServers, o.LookupEnv)
	if err != nil {
		return nil, nil, err
	}
	manager, err := mcp.NewManager(mcp.Options{
		Config: o.Config, Servers: servers, HTTPClient: o.HTTPClient,
		Dir: turn.Cwd, Tokens: o.Tokens, Pool: pool,
		Logger: o.Logger, Tracer: o.Tracer,
		ClientName: "acpharness", ClientVersion: o.Version,
	})
	if err != nil {
		return nil, nil, err
	}
	frozen, err := manager.Tools(ctx, taken)
	if err != nil {
		return nil, nil, errors.Join(err, manager.Close())
	}
	return manager, frozen, nil
}
