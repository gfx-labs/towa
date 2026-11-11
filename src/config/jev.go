package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Routes to a Jev decisions endpoint.
const (
	// JevOpenRouter is OpenRouter's decisions endpoint.
	JevOpenRouter = "openrouter"
	// JevTypeSafe is TypeSafe's own endpoint.
	JevTypeSafe = "typesafe"
	// JevAIMLAPI is AIMLAPI's decisions endpoint.
	JevAIMLAPI = "aimlapi"
	// JevCustom is an endpoint at BaseURL that speaks TypeSafe's shape.
	JevCustom = "custom"
)

// Jev configures the decisions endpoint behind the jev_decision tool. An
// empty Route means there is none. src/jev reads it and holds each route's
// endpoint and default model.
type Jev struct {
	// Route is openrouter, typesafe, aimlapi or custom.
	Route string `koanf:"route"`
	// Model is the route's own name for the model. Empty means the route's
	// default. The custom route has no default.
	Model string `koanf:"model"`
	// Account is the id of the account whose credential is sent. Only the
	// custom route may leave it empty, for an endpoint that wants none.
	Account string `koanf:"account"`
	// BaseURL is the root of the custom route's endpoint. It must be https,
	// or http to a loopback, private or tailnet host.
	BaseURL string `koanf:"base_url"`
	// Referer and Title name the application to OpenRouter, as they do for
	// a provider. Neither is sent when empty.
	Referer string `koanf:"referer"`
	Title   string `koanf:"title"`
	// Timeout bounds a decision that names no timeout of its own.
	Timeout time.Duration `koanf:"timeout"`
	// MaxTimeout is the longest timeout a decision may ask for.
	MaxTimeout time.Duration `koanf:"max_timeout"`
	// MaxRequestBytes is the largest request body that is sent. A larger
	// one is an error before anything is sent.
	MaxRequestBytes int64 `koanf:"max_request_bytes"`
	// MaxResponseBytes is the largest response body that is read.
	MaxResponseBytes int64 `koanf:"max_response_bytes"`
	// MaxTextChars is the most characters the instructions, or one
	// option's description, may hold.
	MaxTextChars int `koanf:"max_text_chars"`
}

// The limits are jcode's. docs/notes/2026-11-11-webfetch-and-jev.md gives
// the ref, file and line of each.
var jevDefaults = map[string]any{
	"jev.route":    "",
	"jev.model":    "",
	"jev.account":  "",
	"jev.base_url": "",
	"jev.referer":  "",
	"jev.title":    "",
	// DEFAULT_TIMEOUT_MS and MAX_TIMEOUT_MS of the jev_decision tool.
	"jev.timeout":     "10s",
	"jev.max_timeout": "30s",
	// MAX_REQUEST_BYTES and MAX_RESPONSE_BYTES of jev.rs.
	"jev.max_request_bytes":  80 << 10,
	"jev.max_response_bytes": 256 << 10,
	// MAX_TEXT_CHARS of the jev_decision tool.
	"jev.max_text_chars": 4000,
}

func (j Jev) validate() error {
	switch {
	case j.Timeout <= 0:
		return fmt.Errorf("jev.timeout: %s is not positive", j.Timeout)
	case j.MaxTimeout < j.Timeout:
		return fmt.Errorf("jev.max_timeout: %s is under jev.timeout, %s", j.MaxTimeout, j.Timeout)
	case j.MaxRequestBytes <= 0:
		return fmt.Errorf("jev.max_request_bytes: %d is not positive", j.MaxRequestBytes)
	case j.MaxResponseBytes <= 0:
		return fmt.Errorf("jev.max_response_bytes: %d is not positive", j.MaxResponseBytes)
	case j.MaxTextChars <= 0:
		return fmt.Errorf("jev.max_text_chars: %d is not positive", j.MaxTextChars)
	}

	switch j.Route {
	case "":
		// A key set without a route is a config that was meant to do
		// something and does nothing.
		for key, value := range map[string]string{
			"model": j.Model, "account": j.Account, "base_url": j.BaseURL, "referer": j.Referer, "title": j.Title,
		} {
			if value != "" {
				return fmt.Errorf("jev.%s is %q and jev.route is empty", key, value)
			}
		}
		return nil
	case JevOpenRouter, JevTypeSafe, JevAIMLAPI:
		if j.BaseURL != "" {
			return fmt.Errorf("jev.base_url: the %s route has a fixed endpoint; use the %s route for another", j.Route, JevCustom)
		}
		if j.Account == "" {
			return fmt.Errorf("jev.account: the %s route needs an account", j.Route)
		}
	case JevCustom:
		if j.Model == "" {
			return fmt.Errorf("jev.model: the %s route has no default model", JevCustom)
		}
		if err := validateJevBaseURL(j.BaseURL); err != nil {
			return err
		}
	default:
		return fmt.Errorf("jev.route: %q is not %s, %s, %s or %s",
			j.Route, JevOpenRouter, JevTypeSafe, JevAIMLAPI, JevCustom)
	}
	if j.Route != JevOpenRouter && (j.Referer != "" || j.Title != "") {
		return fmt.Errorf("jev.referer and jev.title are sent only on the %s route", JevOpenRouter)
	}
	return nil
}

// validateJevBaseURL applies the rule jcode applies to its own custom
// endpoint: a credential and the decision's state go to it, so plain http
// is accepted only where the network is the operator's.
func validateJevBaseURL(base string) error {
	if base == "" {
		return fmt.Errorf("jev.base_url: the %s route needs one", JevCustom)
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return fmt.Errorf("jev.base_url: %q is not a URL with a host", base)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("jev.base_url: %q holds credentials, a query or a fragment", base)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !jevLocalHost(u.Hostname()) {
			return fmt.Errorf("jev.base_url: %q is http to a host that is not loopback, private or on a tailnet", base)
		}
	default:
		return fmt.Errorf("jev.base_url: %q is not an http or https URL", base)
	}
	return nil
}

// cgnat is the range tailnet addresses come from.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// jevLocalHost says the host is loopback, in a private range, or named as a
// tailnet or mDNS host.
func jevLocalHost(host string) bool {
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".ts.net") || strings.HasSuffix(lower, ".local") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate() || cgnat.Contains(addr)
}
