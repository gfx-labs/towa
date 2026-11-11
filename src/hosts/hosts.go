// Package hosts lists the hosts a resolved configuration makes the process
// contact, each with the config key that names it. A runner builds an
// egress allowlist from the list.
//
// No URL is written here. Each comes from the configuration or from the
// package that owns it, and what cannot be known from them is reported in
// Report.NotCovered and never guessed.
package hosts

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"slices"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
)

// The standard variables that name where the OTLP exporters send to.
const (
	EnvOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPTracesEndpoint  = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	EnvOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	EnvOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// Host is one address the process contacts.
type Host struct {
	// Address is the host and port, as net.JoinHostPort writes them.
	Address string
	// Key is the config key that names the host. For a host named by an
	// environment variable it is the config key that turns the contact on,
	// then the variable.
	Key string
}

// Inputs is what List needs besides the configuration.
type Inputs struct {
	// Flows are the OAuth flows by provider, from auth.Flows.
	Flows map[string]auth.Flow
	// OTLP holds the values of the OTEL_EXPORTER_OTLP_* endpoint
	// variables, empty when unset.
	OTLP OTLP
}

// OTLP holds the endpoint variables of the OTLP exporters.
type OTLP struct {
	Endpoint, TracesEndpoint, MetricsEndpoint, LogsEndpoint string
}

// Report is the result of List.
type Report struct {
	// Hosts is sorted by address, then key, without repeats.
	Hosts []Host
	// NotCovered says, one entry each, what the process may contact that
	// Hosts does not hold.
	NotCovered []string
}

// List returns the hosts cfg makes the process contact.
func List(cfg config.Config, in Inputs) (Report, error) {
	var r Report
	add := func(key, rawURL string) error {
		address, err := address(rawURL)
		if err != nil {
			return fmt.Errorf("hosts: %s: %w", key, err)
		}
		r.Hosts = append(r.Hosts, Host{Address: address, Key: key})
		return nil
	}

	configured := map[string]bool{}
	for i, p := range cfg.Providers {
		configured[p.ID] = true
		if err := add(fmt.Sprintf("providers[%d].base_url", i), p.BaseURL); err != nil {
			return Report{}, err
		}
	}
	for i, a := range cfg.Accounts {
		at := fmt.Sprintf("accounts[%d]", i)
		// A proxied account contacts its provider like any other. Only
		// its credential is held elsewhere.
		if !configured[a.Provider] {
			r.NotCovered = append(r.NotCovered, fmt.Sprintf(
				"%s.provider: the base URL of the built-in provider %q is not listed yet", at, a.Provider))
		}
		// The harness logs in and refreshes only an oauth account. A
		// proxied one has its token refreshed by the proxy.
		if a.Kind != config.AccountOAuth {
			continue
		}
		flow, ok := in.Flows[a.Provider]
		if !ok {
			return Report{}, fmt.Errorf("hosts: %s.provider: %q has no OAuth flow", at, a.Provider)
		}
		// The authorize page is opened by the user's browser, which may
		// be on this host.
		if err := add(at+".provider", flow.AuthorizeURL); err != nil {
			return Report{}, err
		}
		if err := add(at+".provider", flow.TokenURL); err != nil {
			return Report{}, err
		}
	}

	remote := false
	for i, s := range cfg.MCP.Servers {
		at := fmt.Sprintf("mcp.servers[%d]", i)
		if s.Transport == config.MCPTransportStdio {
			r.NotCovered = append(r.NotCovered, fmt.Sprintf(
				"%s: %q is a stdio server, and what the command %q contacts cannot be known", at, s.Name, s.Command))
			continue
		}
		remote = true
		if err := add(at+".url", s.URL); err != nil {
			return Report{}, err
		}
	}
	if remote {
		r.NotCovered = append(r.NotCovered,
			"mcp.servers: a remote server that asks for OAuth names its authorization server when it is contacted, so that host is not listed")
	}

	if cfg.Telemetry.OTel {
		signals := []struct{ name, value string }{
			{EnvOTLPTracesEndpoint, in.OTLP.TracesEndpoint},
			{EnvOTLPMetricsEndpoint, in.OTLP.MetricsEndpoint},
		}
		if cfg.Telemetry.Logs.OTel.Enabled {
			signals = append(signals, struct{ name, value string }{EnvOTLPLogsEndpoint, in.OTLP.LogsEndpoint})
		}
		for _, signal := range signals {
			name, value := signal.name, signal.value
			if value == "" {
				name, value = EnvOTLPEndpoint, in.OTLP.Endpoint
			}
			if value == "" {
				// The exporters then use their own default, which is
				// theirs to state.
				r.NotCovered = append(r.NotCovered, fmt.Sprintf(
					"telemetry.otel: neither %s nor %s is set, so the exporter's own default endpoint is used and is not listed",
					signal.name, EnvOTLPEndpoint))
				continue
			}
			if err := add("telemetry.otel:"+name, value); err != nil {
				return Report{}, err
			}
		}
	}

	r.NotCovered = append(r.NotCovered,
		"embeddings: not listed yet",
		"jev: not listed yet",
		"webfetch: the model chooses the hosts, so they cannot be listed",
		"session/new: MCP servers a client names for a session are not in the configuration",
		"hooks and the shell tool: what a command contacts cannot be known",
	)

	slices.SortFunc(r.Hosts, func(a, b Host) int {
		return cmp.Or(cmp.Compare(a.Address, b.Address), cmp.Compare(a.Key, b.Key))
	})
	r.Hosts = slices.Compact(r.Hosts)
	return r, nil
}

// address returns the host and port a URL is reached at. A URL without a
// port has the port of its scheme.
func address(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("%q has no host", rawURL)
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https", "wss":
			port = "443"
		case "http", "ws":
			port = "80"
		default:
			return "", fmt.Errorf("%q has no port and its scheme %q has no known one", rawURL, u.Scheme)
		}
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}
