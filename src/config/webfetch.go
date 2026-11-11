package config

import "time"

// WebFetch configures the fetch of one URL for the model.
type WebFetch struct {
	Enabled bool `koanf:"enabled"`
	// Timeout bounds one fetch, from the request to the last byte of the
	// body, across redirects.
	Timeout time.Duration `koanf:"timeout" validate:"gt=0"`
	// MaxBodyBytes is the largest response body that is read. A larger one
	// is an error: half an HTML document does not convert to half a page.
	MaxBodyBytes int64 `koanf:"max_body_bytes" validate:"gt=0"`
	// MaxResultBytes is the most text one fetch returns. What is over it is
	// left out, and the result says how many bytes that was.
	MaxResultBytes int64 `koanf:"max_result_bytes" validate:"gt=0"`
	// UserAgent is sent with the request.
	UserAgent string `koanf:"user_agent" validate:"required"`
}

// The three limits are jcode's, from crates/jcode-app-core/src/tool/webfetch.rs
// at 8e11eb23e: DEFAULT_TIMEOUT (line 17), MAX_SIZE (line 9) and
// MAX_OUTPUT_CHARS (line 12), which despite its name counts bytes.
var webfetchDefaults = map[string]any{
	"webfetch.enabled":          true,
	"webfetch.timeout":          "30s",
	"webfetch.max_body_bytes":   5 << 20,
	"webfetch.max_result_bytes": 40000,
	// Go's own default names the HTTP library, which some sites refuse.
	"webfetch.user_agent": "acpharness",
}

func (w WebFetch) validate() error {
	return validateFields("webfetch", w)
}
