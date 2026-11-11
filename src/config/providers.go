package config

import (
	"fmt"
	"net/url"
	"slices"
)

// Provider is an endpoint that is not built in, such as an OpenAI-compatible
// API. The package that reads it adds the fields it needs here.
type Provider struct {
	// ID names the provider in accounts and model chains.
	ID   string `koanf:"id"`
	Kind string `koanf:"kind"`
	// BaseURL is the endpoint's root, such as https://openrouter.ai/api/v1.
	BaseURL string `koanf:"base_url"`

	// Referer and Title name the application to the provider. They are sent
	// as the HTTP-Referer and X-Title headers, and neither is sent when
	// empty. OpenRouter uses them for attribution.
	Referer string `koanf:"referer"`
	Title   string `koanf:"title"`
	// CacheKeyField is the request body member that carries the session id
	// for the provider's prompt cache. Nothing is sent when it is empty,
	// because an endpoint may reject a member it does not know.
	CacheKeyField string `koanf:"cache_key_field"`
	// OpenRouter holds what only OpenRouter understands. It applies to a
	// provider of kind chat.
	OpenRouter OpenRouter `koanf:"openrouter"`
}

// Kinds of a provider: the wire protocol it speaks.
const (
	ProviderChat      = "chat"
	ProviderResponses = "responses"
)

// Values of Provider.CacheKeyField.
const (
	// CacheKeyPromptCacheKey is OpenAI's member.
	CacheKeyPromptCacheKey = "prompt_cache_key"
	// CacheKeySessionID is OpenRouter's member. It also keeps a session on
	// the upstream provider that holds its cache.
	CacheKeySessionID = "session_id"
)

// OpenRouter configures the OpenRouter extensions to Chat Completions.
type OpenRouter struct {
	// Enabled says the endpoint is OpenRouter. The reasoning effort is then
	// sent in OpenRouter's reasoning object, the rate limit reset header is
	// read as OpenRouter defines it, and a response's cost is reported.
	Enabled   bool                `koanf:"enabled"`
	Routing   OpenRouterRouting   `koanf:"routing"`
	Reasoning OpenRouterReasoning `koanf:"reasoning"`
}

// OpenRouterRouting is the request's provider object: which upstream
// providers OpenRouter may send the request to. A field that is not set is
// not sent, and OpenRouter's default applies.
type OpenRouterRouting struct {
	// Order lists provider slugs to try in order.
	Order []string `koanf:"order"`
	// Only and Ignore allow and exclude provider slugs.
	Only   []string `koanf:"only"`
	Ignore []string `koanf:"ignore"`
	// AllowFallbacks says whether a provider outside Order may be used.
	AllowFallbacks *bool `koanf:"allow_fallbacks"`
	// RequireParameters limits routing to providers that support every
	// parameter of the request.
	RequireParameters *bool `koanf:"require_parameters"`
	// DataCollection is "allow" or "deny".
	DataCollection string `koanf:"data_collection"`
	// ZDR limits routing to zero data retention endpoints.
	ZDR *bool `koanf:"zdr"`
	// Sort is "price", "throughput" or "latency".
	Sort string `koanf:"sort"`
	// Quantizations lists the quantization levels to allow, such as "fp8".
	Quantizations []string `koanf:"quantizations"`
	// MaxPrice is the most to pay, in USD per million tokens for "prompt"
	// and "completion" and per unit for "request" and "image".
	MaxPrice map[string]float64 `koanf:"max_price"`
	// PreferredMinThroughputTPS is in tokens per second and
	// PreferredMaxLatencySeconds in seconds. Zero is not sent.
	PreferredMinThroughputTPS  float64 `koanf:"preferred_min_throughput_tps"`
	PreferredMaxLatencySeconds float64 `koanf:"preferred_max_latency_seconds"`
}

// OpenRouterReasoning is the part of the request's reasoning object that
// does not come from the request's effort.
type OpenRouterReasoning struct {
	// Exclude asks OpenRouter not to return the reasoning text. The model
	// still reasons and the tokens are still billed.
	Exclude bool `koanf:"exclude"`
}

// Values of OpenRouterRouting fields.
var (
	openRouterSorts          = []string{"", "price", "throughput", "latency"}
	openRouterDataCollection = []string{"", "allow", "deny"}
	openRouterMaxPriceKeys   = []string{"prompt", "completion", "request", "image"}
)

func (c *Config) validateProviders() error {
	seen := map[string]bool{}
	for i, p := range c.Providers {
		at := fmt.Sprintf("providers[%d]", i)
		if seen[p.ID] {
			return fmt.Errorf("%s.id: %q is used by more than one provider", at, p.ID)
		}
		if err := p.Validate(at); err != nil {
			return err
		}
		seen[p.ID] = true
	}
	return nil
}

// Validate reports a provider the program cannot act on. at names the
// provider in the error, as a config key.
func (p Provider) Validate(at string) error {
	switch {
	case p.ID == "":
		return fmt.Errorf("%s.id is empty", at)
	case p.Kind != ProviderChat && p.Kind != ProviderResponses:
		return fmt.Errorf("%s.kind: %q is not %q or %q", at, p.Kind, ProviderChat, ProviderResponses)
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s.base_url: %q is not an http or https URL", at, p.BaseURL)
	}
	switch p.CacheKeyField {
	case "", CacheKeyPromptCacheKey, CacheKeySessionID:
	default:
		return fmt.Errorf("%s.cache_key_field: %q is not %q or %q", at, p.CacheKeyField, CacheKeyPromptCacheKey, CacheKeySessionID)
	}
	return p.OpenRouter.validate(at+".openrouter", p.Kind)
}

func (o OpenRouter) validate(at, kind string) error {
	r := o.Routing
	routingSet := len(r.Order)+len(r.Only)+len(r.Ignore)+len(r.Quantizations)+len(r.MaxPrice) > 0 ||
		r.AllowFallbacks != nil || r.RequireParameters != nil || r.ZDR != nil || r.DataCollection != "" || r.Sort != "" ||
		r.PreferredMinThroughputTPS != 0 || r.PreferredMaxLatencySeconds != 0
	switch {
	case o.Enabled && kind != ProviderChat:
		return fmt.Errorf("%s.enabled is set for a provider of kind %q, and applies only to %q", at, kind, ProviderChat)
	case !o.Enabled && (routingSet || o.Reasoning.Exclude):
		return fmt.Errorf("%s has settings and %s.enabled is false, so they would not be sent", at, at)
	case !slices.Contains(openRouterSorts, r.Sort):
		return fmt.Errorf("%s.routing.sort: %q is not one of %q", at, r.Sort, openRouterSorts[1:])
	case !slices.Contains(openRouterDataCollection, r.DataCollection):
		return fmt.Errorf("%s.routing.data_collection: %q is not one of %q", at, r.DataCollection, openRouterDataCollection[1:])
	case r.PreferredMinThroughputTPS < 0:
		return fmt.Errorf("%s.routing.preferred_min_throughput_tps: %v is negative", at, r.PreferredMinThroughputTPS)
	case r.PreferredMaxLatencySeconds < 0:
		return fmt.Errorf("%s.routing.preferred_max_latency_seconds: %v is negative", at, r.PreferredMaxLatencySeconds)
	}
	for key, price := range r.MaxPrice {
		if !slices.Contains(openRouterMaxPriceKeys, key) {
			return fmt.Errorf("%s.routing.max_price.%s is not one of %q", at, key, openRouterMaxPriceKeys)
		}
		if price < 0 {
			return fmt.Errorf("%s.routing.max_price.%s: %v is negative", at, key, price)
		}
	}
	return nil
}
