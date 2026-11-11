package config

import "time"

// Config keys of the per-turn limits. A turn that stops on one names it in
// its harness.Stop, so a client can tell which to raise.
const (
	KeyMaxToolCalls    = "limits.max_tool_calls"
	KeyMaxWall         = "limits.max_wall"
	KeyMaxInputTokens  = "limits.max_input_tokens"
	KeyMaxOutputTokens = "limits.max_output_tokens"
	KeyMaxStopDenials  = "limits.max_stop_denials"
)

// Limits configures what one prompt turn may use, and how a failed model
// request is retried. Zero means no limit.
type Limits struct {
	// MaxToolCalls is how many tool calls one turn may run.
	MaxToolCalls int64 `koanf:"max_tool_calls" validate:"gte=0"`
	// MaxWall is how long one turn may take.
	MaxWall time.Duration `koanf:"max_wall" validate:"gte=0"`
	// MaxInputTokens bounds the tokens sent over all of a turn's requests,
	// counting the ones read from and written to the cache.
	MaxInputTokens int64 `koanf:"max_input_tokens" validate:"gte=0"`
	// MaxOutputTokens bounds the tokens the model writes over all of a
	// turn's requests.
	MaxOutputTokens int64 `koanf:"max_output_tokens" validate:"gte=0"`
	// MaxStopDenials is how many times in one turn the stop hooks may keep
	// the turn going by denying its end. The next deny ends the turn. Zero
	// is not "no limit" here: it ends the turn on the first deny, because a
	// hook that always denies would otherwise run a turn forever.
	MaxStopDenials int64 `koanf:"max_stop_denials" validate:"gte=0"`
	Retry          Retry `koanf:"retry"`
}

// Retry configures how the router retries a request on one chain entry
// before it moves to the next.
type Retry struct {
	// MaxRetries is how many times a request is sent again to the same
	// entry. Zero turns retrying off.
	MaxRetries int `koanf:"max_retries" validate:"gte=0"`
	// InitialBackoff is the wait before the first retry. Each later wait is
	// Multiplier times the one before, up to MaxBackoff.
	InitialBackoff time.Duration `koanf:"initial_backoff" validate:"gt=0"`
	MaxBackoff     time.Duration `koanf:"max_backoff" validate:"gtefield=InitialBackoff"`
	Multiplier     float64       `koanf:"multiplier" validate:"gte=1,finite"`
	// Jitter is the largest fraction a wait is shortened by, chosen at
	// random for each wait, so that many processes do not retry together.
	Jitter float64 `koanf:"jitter" validate:"gte=0,lte=1"`
	// MaxRetryAfter is the longest wait a provider may ask for and still be
	// waited for. A longer one counts as retries exhausted.
	MaxRetryAfter time.Duration `koanf:"max_retry_after" validate:"gte=0"`
}

// The turn limits default to off, as in the sketch in docs/design.md: a
// limit is a choice of whoever runs the harness. The retry defaults are the
// ones the Anthropic and OpenAI SDKs ship with (their generated clients
// share them): 2 retries, 0.5s first wait doubling to at most 8s, a wait
// shortened by up to 25%, and a retry-after honoured up to 60s. These were
// written from memory and not re-read from the SDK sources.
// max_stop_denials is 3 by choice: no upstream harness bounds it and nothing
// was measured. It is enough for a hook to ask for a fix and see it made.
var limitsDefaults = map[string]any{
	KeyMaxToolCalls:                "0",
	KeyMaxWall:                     "0s",
	KeyMaxInputTokens:              "0",
	KeyMaxOutputTokens:             "0",
	KeyMaxStopDenials:              "3",
	"limits.retry.max_retries":     "2",
	"limits.retry.initial_backoff": "500ms",
	"limits.retry.max_backoff":     "8s",
	"limits.retry.multiplier":      "2",
	"limits.retry.jitter":          "0.25",
	"limits.retry.max_retry_after": "60s",
}

func (l Limits) validate() error {
	return validateFields("limits", l)
}

// Validate reports retry settings the router cannot act on.
func (r Retry) Validate() error {
	return validateFields("limits.retry", r)
}
