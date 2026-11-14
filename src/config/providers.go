package config

import (
	"fmt"
	"net/url"
)

// Provider is an endpoint that is not built in, such as an OpenAI-compatible
// API. The package that reads it adds the fields it needs here.
type Provider struct {
	// ID names the provider in accounts and model chains.
	ID   string `json:"id" validate:"required"`
	Kind string `json:"kind" validate:"oneof=chat responses"`
	// BaseURL is the endpoint's root, such as https://openrouter.ai/api/v1.
	BaseURL string `json:"base_url"`

	// Referer and Title name the application to the provider. They are sent
	// as the HTTP-Referer and X-OpenRouter-Title headers, and neither is sent when
	// empty. OpenRouter uses them for attribution.
	Referer string `json:"referer"`
	Title   string `json:"title"`
	// CacheKeyField is the request body member that carries the session id
	// for the provider's prompt cache. Nothing is sent when it is empty,
	// because an endpoint may reject a member it does not know.
	CacheKeyField string `json:"cache_key_field" validate:"omitempty,oneof=prompt_cache_key session_id"`
	// OpenRouter holds what only OpenRouter understands. It applies to a
	// provider of kind chat.
	OpenRouter OpenRouter `json:"openrouter"`
	// Models says what the endpoint's models are, for the ones nothing else
	// describes: a gateway names a model in its own way and may serve it
	// with other limits than the model's maker does.
	Models []ProviderModel `json:"models" validate:"unique=ID,dive"`
}

// ProviderModel is what a provider's entry says about one of its models. A
// figure that is zero is not set, and comes from where it would without the
// entry.
type ProviderModel struct {
	// ID is the model as a chain entry names it, such as
	// "deepseek/deepseek-v4.1-flash-1017".
	ID string `json:"id" validate:"required"`
	// ContextWindow is the context size in tokens.
	ContextWindow int64 `json:"context_window" validate:"gte=0"`
	// MaxOutputTokens is the most tokens one response may hold. A chat
	// provider is sent it as the limit of every request.
	MaxOutputTokens int64 `json:"max_output_tokens" validate:"gte=0"`
	// ReasoningEfforts are the efforts a session may choose for the model,
	// in the provider's own words. None means the session has no choice.
	ReasoningEfforts []string `json:"reasoning_efforts" validate:"unique,dive,required"`
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
	Enabled   bool                `json:"enabled"`
	Routing   OpenRouterRouting   `json:"routing"`
	Reasoning OpenRouterReasoning `json:"reasoning"`
}

// OpenRouterRouting is the request's provider object: which upstream
// providers OpenRouter may send the request to. A field that is not set is
// not sent, and OpenRouter's default applies.
type OpenRouterRouting struct {
	// Order lists provider slugs to try in order.
	Order []string `json:"order"`
	// Only and Ignore allow and exclude provider slugs.
	Only   []string `json:"only"`
	Ignore []string `json:"ignore"`
	// AllowFallbacks says whether a provider outside Order may be used.
	AllowFallbacks *bool `json:"allow_fallbacks"`
	// RequireParameters limits routing to providers that support every
	// parameter of the request.
	RequireParameters *bool `json:"require_parameters"`
	// DataCollection is "allow" or "deny".
	DataCollection string `json:"data_collection" validate:"omitempty,oneof=allow deny"`
	// ZDR limits routing to zero data retention endpoints.
	ZDR *bool `json:"zdr"`
	// Sort is "price", "throughput" or "latency".
	Sort string `json:"sort" validate:"omitempty,oneof=price throughput latency"`
	// Quantizations lists the quantization levels to allow, such as "fp8".
	Quantizations []string `json:"quantizations"`
	// MaxPrice is the most to pay, in USD per million tokens for "prompt"
	// and "completion" and per unit for "request" and "image".
	MaxPrice map[string]float64 `json:"max_price" validate:"dive,keys,oneof=prompt completion request image,endkeys,gte=0,finite"`
	// PreferredMinThroughputTPS is in tokens per second and
	// PreferredMaxLatencySeconds in seconds. Zero is not sent.
	PreferredMinThroughputTPS  float64 `json:"preferred_min_throughput_tps" validate:"gte=0,finite"`
	PreferredMaxLatencySeconds float64 `json:"preferred_max_latency_seconds" validate:"gte=0,finite"`
}

// OpenRouterReasoning is the part of the request's reasoning object that
// does not come from the request's effort.
type OpenRouterReasoning struct {
	// Exclude asks OpenRouter not to return the reasoning text. The model
	// still reasons and the tokens are still billed.
	Exclude bool `json:"exclude"`
}

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
	if err := validateFields(at, p); err != nil {
		return err
	}
	u, err := url.Parse(p.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%s.base_url: must be an http or https URL with a host", at)
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
	}
	return nil
}
