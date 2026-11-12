package config

import "time"

// WebSearch configures the web search tool. The search is done by this
// process against Endpoint, not by the model's provider.
//
// The endpoint, region and result counts are jcode's, from
// crates/jcode-app-core/src/tool/websearch.rs at 037163c7b (lines 219, 229
// and 81). The timeout and body limit are webfetch's. The plain user agent
// was answered with results on 2026-11-11, so no browser is imitated.
// hosted and hosted_max_uses are jcode's prefer_native and native_max_uses
// (crates/jcode-config-types/src/web_search.rs, lines 168 and 178).
type WebSearch struct {
	Enabled bool `name:"enabled" help:"Give the model the tool that searches the web."`
	// Hosted uses the model provider's own search where it has one: the
	// built-in claude and codex providers. The provider then runs the
	// search and this process's websearch tool is left out of the request.
	// Other providers always get this process's tool.
	Hosted bool `name:"hosted" default:"true" help:"With websearch.enabled, use the model provider's own search on the built-in claude and codex providers, and leave this process's tool out of those requests. On by default."`
	// HostedMaxUses is the most searches one Anthropic request may make.
	// Zero sends no cap. The Responses API has no such setting.
	HostedMaxUses int `name:"hosted_max_uses" default:"5" validate:"gte=0" help:"Most searches one Anthropic request may make with the provider's own search. 0 sends no cap."`
	// Endpoint is DuckDuckGo's HTML search page, or a server that answers
	// the same form with the same markup.
	Endpoint string `name:"endpoint" default:"https://html.duckduckgo.com/html/" placeholder:"URL" validate:"required,http_url" help:"DuckDuckGo's HTML search page, or a server that answers the same form with the same markup."`
	// Region is the form's kl value, which picks the results' region and
	// language.
	Region string `name:"region" default:"us-en" validate:"required" help:"The form's kl value, which picks the results' region and language."`
	// Timeout bounds one search, from the request to the last byte.
	Timeout time.Duration `name:"timeout" default:"30s" validate:"gt=0" help:"How long one search may take, from the request to the last byte."`
	// MaxBodyBytes is the largest result page that is read.
	MaxBodyBytes int64 `name:"max_body_bytes" default:"5242880" validate:"gt=0" help:"Largest result page that is read."`
	// DefaultResults is how many results a call gets when it names no
	// number, and MaxResults the most it may ask for.
	DefaultResults int `name:"default_results" default:"8" validate:"gt=0,ltefield=MaxResults" help:"Results a call gets when it names no number."`
	MaxResults     int `name:"max_results" default:"20" validate:"gt=0" help:"Most results a call may ask for."`
	// UserAgent is sent with the request.
	UserAgent string `name:"user_agent" default:"towa" validate:"required" help:"User-Agent sent with the request."`
}

func (w WebSearch) validate() error {
	return validateFields("websearch", w)
}
