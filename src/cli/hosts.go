package cli

import (
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hosts"
	"github.com/gfx-labs/acpharness/src/jev"
)

// hostsEnv is what the hosts command takes from the packages that own a
// URL. A test points them at hosts it names.
type hostsEnv struct {
	flows   map[string]auth.Flow
	builtIn hosts.BuiltIn
}

type hostsCmd struct{}

// Run prints the hosts. Standard output holds one "host:port<TAB>key" line
// per host and nothing else, so that it can be piped into an allowlist.
// What the list does not cover goes to standard error.
func (hostsCmd) Run(loaded *config.Loaded, h Host, env hostsEnv) error {
	cfg := loaded.Config
	jevEndpoint, err := jevEndpoint(cfg.Jev)
	if err != nil {
		return err
	}
	tokenEndpoints, err := mcpTokenEndpoints(h, cfg.MCP)
	if err != nil {
		return err
	}
	report, err := hosts.List(cfg, hosts.Inputs{
		Flows:   env.flows,
		BuiltIn: env.builtIn,
		OTLP: hosts.OTLP{
			Endpoint:        lookup(h.Environ, hosts.EnvOTLPEndpoint),
			TracesEndpoint:  lookup(h.Environ, hosts.EnvOTLPTracesEndpoint),
			MetricsEndpoint: lookup(h.Environ, hosts.EnvOTLPMetricsEndpoint),
			LogsEndpoint:    lookup(h.Environ, hosts.EnvOTLPLogsEndpoint),
		},
		JevEndpoint:       jevEndpoint,
		MCPTokenEndpoints: tokenEndpoints,
	})
	if err != nil {
		return err
	}
	for _, host := range report.Hosts {
		if _, err := fmt.Fprintf(h.Stdout, "%s\t%s\n", host.Address, host.Key); err != nil {
			return err
		}
	}
	for _, gap := range report.NotCovered {
		if _, err := fmt.Fprintf(h.Stderr, "hosts: not covered: %s\n", gap); err != nil {
			return err
		}
	}
	return nil
}

// jevEndpoint returns the route endpoint owned by src/jev, or "" when disabled.
func jevEndpoint(cfg config.Jev) (string, error) {
	endpoint, err := jev.Endpoint(cfg)
	if errors.Is(err, jev.ErrDisabled) {
		return "", nil
	}
	return endpoint, err
}

// mcpTokenEndpoints returns, by server name, the token endpoint a refresh
// of each remote MCP server's stored login would contact.
func mcpTokenEndpoints(h Host, cfg config.MCP) (map[string]string, error) {
	endpoints := map[string]string{}
	var store *auth.Store
	for _, s := range cfg.Servers {
		if s.Transport == config.MCPTransportStdio {
			continue
		}
		if store == nil {
			var err error
			if store, err = authStore(h); err != nil {
				return nil, fmt.Errorf("finding the stored login of MCP server %q: %w", s.Name, err)
			}
		}
		tokens, err := store.LoadMCP(s.Name)
		if errors.Is(err, auth.ErrNotLoggedIn) {
			continue
		}
		if err != nil {
			return nil, err
		}
		// The MCP client uses a login only for the URL it was made for, and
		// sends no refresh after one was refused or without a refresh token.
		if tokens.ServerURL != s.URL || tokens.RefreshToken == "" || tokens.TokenEndpoint == "" || tokens.RefreshRejectedAtUnixMs != 0 {
			continue
		}
		endpoints[s.Name] = tokens.TokenEndpoint
	}
	return endpoints, nil
}
