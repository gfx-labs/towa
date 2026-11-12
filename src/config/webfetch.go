package config

import "time"

// WebFetch configures the fetch of one URL for the model. The three limits
// are jcode's, from crates/jcode-app-core/src/tool/webfetch.rs at
// 8e11eb23e: DEFAULT_TIMEOUT (line 17), MAX_SIZE (line 9) and
// MAX_OUTPUT_CHARS (line 12), which despite its name counts bytes.
type WebFetch struct {
	Enabled bool `name:"enabled" help:"Give the model the tool that fetches one URL."`
	// Timeout bounds one fetch, from the request to the last byte of the
	// body, across redirects.
	Timeout time.Duration `name:"timeout" default:"30s" validate:"gt=0" help:"How long one fetch may take, from the request to the last byte of the body, across redirects."`
	// MaxBodyBytes is the largest response body that is read. A larger one
	// is an error: half an HTML document does not convert to half a page.
	MaxBodyBytes int64 `name:"max_body_bytes" default:"5242880" validate:"gt=0" help:"Largest response body that is read. A larger one is an error."`
	// MaxResultBytes is the most text one fetch returns. What is over it is
	// left out, and the result says how many bytes that was.
	MaxResultBytes int64 `name:"max_result_bytes" default:"40000" validate:"gt=0" help:"Most text one fetch returns. The rest is left out and the result says how much."`
	// UserAgent is sent with the request. Go's own default names the HTTP
	// library, which some sites refuse.
	UserAgent string `name:"user_agent" default:"acpharness" validate:"required" help:"User-Agent sent with the request."`
}

func (w WebFetch) validate() error {
	return validateFields("webfetch", w)
}
