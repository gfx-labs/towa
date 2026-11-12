package config

import "time"

// WebSearch configures the web search tool. The search is done by this
// process against Endpoint, not by the model's provider.
type WebSearch struct {
	Enabled bool `koanf:"enabled"`
	// Hosted uses the model provider's own search where it has one: the
	// built-in claude and codex providers. The provider then runs the
	// search and this process's websearch tool is left out of the request.
	// Other providers always get this process's tool.
	Hosted bool `koanf:"hosted"`
	// HostedMaxUses is the most searches one Anthropic request may make.
	// Zero sends no cap. The Responses API has no such setting.
	HostedMaxUses int `koanf:"hosted_max_uses" validate:"gte=0"`
	// Endpoint is DuckDuckGo's HTML search page, or a server that answers
	// the same form with the same markup.
	Endpoint string `koanf:"endpoint" validate:"required,http_url"`
	// Region is the form's kl value, which picks the results' region and
	// language.
	Region string `koanf:"region" validate:"required"`
	// Timeout bounds one search, from the request to the last byte.
	Timeout time.Duration `koanf:"timeout" validate:"gt=0"`
	// MaxBodyBytes is the largest result page that is read.
	MaxBodyBytes int64 `koanf:"max_body_bytes" validate:"gt=0"`
	// DefaultResults is how many results a call gets when it names no
	// number, and MaxResults the most it may ask for.
	DefaultResults int `koanf:"default_results" validate:"gt=0,ltefield=MaxResults"`
	MaxResults     int `koanf:"max_results" validate:"gt=0"`
	// UserAgent is sent with the request.
	UserAgent string `koanf:"user_agent" validate:"required"`
}

// The endpoint, region and result counts are jcode's, from
// crates/jcode-app-core/src/tool/websearch.rs at 037163c7b (lines 219, 229
// and 81). The timeout and body limit are webfetch's. The plain user agent
// was answered with results on 2026-11-11, so no browser is imitated.
// hosted and hosted_max_uses are jcode's prefer_native and native_max_uses
// (crates/jcode-config-types/src/web_search.rs, lines 168 and 178).
var websearchDefaults = map[string]any{
	"websearch.enabled":         true,
	"websearch.hosted":          true,
	"websearch.hosted_max_uses": 5,
	"websearch.endpoint":        "https://html.duckduckgo.com/html/",
	"websearch.region":          "us-en",
	"websearch.timeout":         "30s",
	"websearch.max_body_bytes":  5 << 20,
	"websearch.default_results": 8,
	"websearch.max_results":     20,
	"websearch.user_agent":      "acpharness",
}

func (w WebSearch) validate() error {
	return validateFields("websearch", w)
}
