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

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
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
	// BuiltIn holds the base URLs of the providers that need no
	// [[providers]] entry, from the packages that own them.
	BuiltIn BuiltIn
	// JevEndpoint is where decisions are sent, from src/sidecar/jev. It is empty
	// when jev.route is.
	JevEndpoint string
	// MCPTokenEndpoints holds, by server name, the token endpoint in the
	// stored login of a remote MCP server. A server has no entry when it
	// has no stored login for its current URL, or one whose refresh token
	// was refused, which is not sent again.
	MCPTokenEndpoints map[string]string
}

// BuiltIn holds the base URLs of the built-in providers.
type BuiltIn struct {
	// Claude is where every account of the claude provider sends.
	Claude string
	// CodexAPIKey and CodexOAuth are where an account of the codex
	// provider sends: the first with an API key, the second with a login.
	CodexAPIKey, CodexOAuth string
}

// url returns the base URL an account of a built-in provider sends to.
func (b BuiltIn) url(a config.Account) (string, error) {
	var u string
	switch a.Provider {
	case config.ProviderClaude:
		u = b.Claude
	case config.ProviderCodex:
		switch a.Kind {
		case config.AccountAPIKey, config.AccountProxiedAPIKey:
			u = b.CodexAPIKey
		case config.AccountOAuth, config.AccountProxiedOAuth:
			u = b.CodexOAuth
		default:
			return "", fmt.Errorf("account kind %q is not known", a.Kind)
		}
	default:
		return "", fmt.Errorf("provider %q is not built in and not configured", a.Provider)
	}
	if u == "" {
		return "", fmt.Errorf("no base URL was given for the built-in provider %q", a.Provider)
	}
	return u, nil
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

// collector gathers the hosts and the notes of one List call.
type collector struct {
	report Report
}

func (c *collector) add(key, rawURL string) error {
	address, err := address(rawURL)
	if err != nil {
		return fmt.Errorf("hosts: %s: %w", key, err)
	}
	c.report.Hosts = append(c.report.Hosts, Host{Address: address, Key: key})
	return nil
}

func (c *collector) note(text string) {
	c.report.NotCovered = append(c.report.NotCovered, text)
}

// List returns the hosts cfg makes the process contact.
func List(cfg config.Config, in Inputs) (Report, error) {
	var c collector
	configured, err := c.providers(cfg)
	if err != nil {
		return Report{}, err
	}
	if err := c.accounts(cfg, in, configured); err != nil {
		return Report{}, err
	}
	if err := c.mcpServers(cfg, in); err != nil {
		return Report{}, err
	}
	if err := c.services(cfg, in); err != nil {
		return Report{}, err
	}
	if err := c.otlp(cfg, in); err != nil {
		return Report{}, err
	}

	if cfg.WebFetch.Enabled {
		c.note("webfetch: the tool fetches any URL the model names, so it reaches any host. An allowlist built from this list alone makes those fetches fail")
	}
	c.note("session/new: MCP servers a client names for a session are not in the configuration")
	c.note("hooks and the shell tool: what a command contacts cannot be known")

	slices.SortFunc(c.report.Hosts, func(a, b Host) int {
		return cmp.Or(cmp.Compare(a.Address, b.Address), cmp.Compare(a.Key, b.Key))
	})
	c.report.Hosts = slices.Compact(c.report.Hosts)
	return c.report, nil
}

// providers adds the base URL of each [[providers]] entry and returns the
// provider ids they configure.
func (c *collector) providers(cfg config.Config) (map[string]bool, error) {
	configured := map[string]bool{}
	for i, p := range cfg.Providers {
		configured[p.ID] = true
		if err := c.add(fmt.Sprintf("providers[%d].base_url", i), p.BaseURL); err != nil {
			return nil, err
		}
	}
	return configured, nil
}

func (c *collector) accounts(cfg config.Config, in Inputs, configured map[string]bool) error {
	for i, a := range cfg.Accounts {
		at := fmt.Sprintf("accounts[%d]", i)
		if a.Stored {
			// A login made it, so no config key names it.
			at = "auth.json:accounts." + a.ID
		}
		// A proxied account contacts its provider like any other. Only
		// its credential is held elsewhere.
		if !configured[a.Provider] {
			base, err := in.BuiltIn.url(a)
			if err != nil {
				return fmt.Errorf("hosts: %s.provider: %w", at, err)
			}
			if err := c.add(at+".provider", base); err != nil {
				return err
			}
		}
		// The harness logs in and refreshes only an oauth account. A
		// proxied one has its token refreshed by the proxy.
		if a.Kind != config.AccountOAuth {
			continue
		}
		flow, ok := in.Flows[a.Provider]
		if !ok {
			return fmt.Errorf("hosts: %s.provider: %q has no OAuth flow", at, a.Provider)
		}
		if err := c.loginFlow(at+".provider", flow); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) loginFlow(key string, flow auth.Flow) error {
	// The authorize page is opened by the user's browser, which may
	// be on this host.
	if err := c.add(key, flow.AuthorizeURL); err != nil {
		return err
	}
	if err := c.add(key, flow.TokenURL); err != nil {
		return err
	}
	// A device code login asks these from this host. Its page is opened
	// on whichever device the user has.
	if flow.Device.UserCodeURL == "" {
		return nil
	}
	for _, u := range []string{flow.Device.UserCodeURL, flow.Device.PollURL} {
		if err := c.add(key, u); err != nil {
			return err
		}
	}
	return nil
}

func (c *collector) mcpServers(cfg config.Config, in Inputs) error {
	remote := false
	for i, s := range cfg.MCP.Servers {
		at := fmt.Sprintf("mcp.servers[%d]", i)
		if s.Transport == config.MCPTransportStdio {
			c.note(fmt.Sprintf(
				"%s: %q is a stdio server, and what the command %q contacts cannot be known", at, s.Name, s.Command))
			continue
		}
		remote = true
		if err := c.add(at+".url", s.URL); err != nil {
			return err
		}
		token, ok := in.MCPTokenEndpoints[s.Name]
		if !ok {
			c.note(fmt.Sprintf(
				"%s: %q has no stored login that a refresh would use, so its authorization server, if it asks for OAuth, is not listed", at, s.Name))
			continue
		}
		if err := c.add(at+".oauth", token); err != nil {
			return err
		}
	}
	if remote {
		c.note("mcp.servers: a stored login holds only its token endpoint, which a refresh contacts. A new mcp login asks the server for its authorization server again, and the metadata and authorize page may be on other hosts")
	}
	return nil
}

// services adds the embeddings, web search and jev endpoints.
func (c *collector) services(cfg config.Config, in Inputs) error {
	if cfg.Embeddings.BaseURL != "" {
		if err := c.add("embeddings.base_url", cfg.Embeddings.BaseURL); err != nil {
			return err
		}
	}
	if cfg.WebSearch.Enabled {
		if err := c.add("websearch.endpoint", cfg.WebSearch.Endpoint); err != nil {
			return err
		}
	}
	if cfg.Jev.Route == "" {
		return nil
	}
	if in.JevEndpoint == "" {
		return fmt.Errorf("hosts: jev.route is %q and no endpoint was given for it", cfg.Jev.Route)
	}
	key := "jev.route"
	if cfg.Jev.Route == config.JevCustom {
		key = "jev.base_url"
	}
	return c.add(key, in.JevEndpoint)
}

func (c *collector) otlp(cfg config.Config, in Inputs) error {
	if !cfg.Telemetry.OTel {
		return nil
	}
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
			c.note(fmt.Sprintf(
				"telemetry.otel: neither %s nor %s is set, so the exporter's own default endpoint is used and is not listed",
				signal.name, EnvOTLPEndpoint))
			continue
		}
		if err := c.add("telemetry.otel:"+name, value); err != nil {
			return err
		}
	}
	return nil
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
