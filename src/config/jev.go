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
// empty Route means there is none. src/sidecar/jev reads it and holds each route's
// endpoint and default model.
//
// The limits are jcode's. docs/notes/2026-11-11-webfetch-and-jev.md gives
// the ref, file and line of each.
type Jev struct {
	// Route is openrouter, typesafe, aimlapi or custom.
	Route string `name:"route" validate:"omitempty,oneof=openrouter typesafe aimlapi custom" help:"Route to a decisions endpoint: openrouter, typesafe, aimlapi or custom. Empty means there is none."`
	// Model is the route's own name for the model. Empty means the route's
	// default. The custom route has no default.
	Model string `name:"model" help:"The route's own name for the model. Empty means the route's default. The custom route has none."`
	// Account is the id of the account whose credential is sent. Only the
	// custom route may leave it empty, for an endpoint that wants none.
	Account string `name:"account" help:"Id of the account whose credential is sent. Only the custom route may leave it empty."`
	// BaseURL is the root of the custom route's endpoint. It must be https,
	// or http to a loopback, private or tailnet host.
	BaseURL string `name:"base_url" placeholder:"URL" help:"Root of the custom route's endpoint: https, or http to a loopback, private or tailnet host."`
	// Referer and Title name the application to OpenRouter, as they do for
	// a provider. Neither is sent when empty.
	Referer string `name:"referer" help:"Names the application to OpenRouter in the HTTP-Referer header. Not sent when empty."`
	Title   string `name:"title" help:"Names the application to OpenRouter in the X-OpenRouter-Title header. Not sent when empty."`
	// Timeout bounds a decision that names no timeout of its own, and
	// MaxTimeout is the longest timeout a decision may ask for:
	// DEFAULT_TIMEOUT_MS and MAX_TIMEOUT_MS of the jev_decision tool.
	Timeout    time.Duration `name:"timeout" default:"10s" validate:"gt=0" help:"Timeout of a decision that names none."`
	MaxTimeout time.Duration `name:"max_timeout" default:"30s" validate:"gtefield=Timeout" help:"Longest timeout a decision may ask for."`
	// MaxRequestBytes is the largest request body that is sent. A larger
	// one is an error before anything is sent. MaxResponseBytes is the
	// largest response body that is read. They are MAX_REQUEST_BYTES and
	// MAX_RESPONSE_BYTES of jev.rs.
	MaxRequestBytes  int64 `name:"max_request_bytes" default:"81920" validate:"gt=0" help:"Largest request body that is sent. A larger one is an error before anything is sent."`
	MaxResponseBytes int64 `name:"max_response_bytes" default:"262144" validate:"gt=0" help:"Largest response body that is read."`
	// MaxTextChars is the most characters the instructions, or one
	// option's description, may hold: MAX_TEXT_CHARS of the jev_decision
	// tool.
	MaxTextChars int `name:"max_text_chars" default:"4000" validate:"gt=0" help:"Most characters the instructions, or one option's description, may hold."`
}

func (j Jev) validate() error {
	if err := validateFields("jev", j); err != nil {
		return err
	}

	switch j.Route {
	case "":
		// A key set without a route is a config that was meant to do
		// something and does nothing.
		for key, value := range map[string]string{
			"model": j.Model, "account": j.Account, "base_url": j.BaseURL, "referer": j.Referer, "title": j.Title,
		} {
			if value != "" {
				return fmt.Errorf("jev.%s is set and jev.route is empty", key)
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
		return fmt.Errorf("jev.base_url: must be a URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("jev.base_url: must not hold credentials, a query or a fragment")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !jevLocalHost(u.Hostname()) {
			return fmt.Errorf("jev.base_url: http requires a loopback, private or tailnet host")
		}
	default:
		return fmt.Errorf("jev.base_url: must be an http or https URL")
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
