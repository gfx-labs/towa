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
// request is retried. Zero means no limit. The turn limits default to off:
// a limit is a choice of whoever runs the harness.
type Limits struct {
	// MaxToolCalls is how many tool calls one turn may run.
	MaxToolCalls int64 `name:"max_tool_calls" default:"0" validate:"gte=0" help:"Tool calls one turn may run. 0 is no limit."`
	// MaxWall is how long one turn may take.
	MaxWall time.Duration `name:"max_wall" default:"0s" validate:"gte=0" help:"How long one turn may take, such as 30m. 0s is no limit."`
	// MaxInputTokens bounds the tokens sent over all of a turn's requests,
	// counting the ones read from and written to the cache.
	MaxInputTokens int64 `name:"max_input_tokens" default:"0" validate:"gte=0" help:"Tokens sent over all requests of one turn, cached ones included. 0 is no limit."`
	// MaxOutputTokens bounds the tokens the model writes over all of a
	// turn's requests.
	MaxOutputTokens int64 `name:"max_output_tokens" default:"0" validate:"gte=0" help:"Tokens the model writes over all requests of one turn. 0 is no limit."`
	// MaxStopDenials is how many times in one turn the stop hooks may keep
	// the turn going by denying its end. The next deny ends the turn. Zero
	// is not "no limit" here: it ends the turn on the first deny, because a
	// hook that always denies would otherwise run a turn forever. The
	// default is 3 by choice: no upstream harness bounds it and nothing was
	// measured. It is enough for a hook to ask for a fix and see it made.
	MaxStopDenials int64 `name:"max_stop_denials" default:"3" validate:"gte=0" help:"Times in one turn that stop hooks may keep the turn going. 0 ends the turn on the first deny."`
	// MaxTodoPokes is how many times a turn that ends with unfinished todo
	// items is sent back to them, while those items and the user's prompt
	// stay the same. Zero, the default, never sends a turn back. jcode's
	// INCOMPLETE_TODO_MAX_ATTEMPTS is 3. It applies only with the todo tool
	// (tools.todo.enabled).
	MaxTodoPokes int64 `name:"max_todo_pokes" default:"0" validate:"gte=0" help:"Times a turn that ends with unfinished todo items is sent back to them, with tools.todo.enabled. 0 never sends it back."`
	Retry        Retry `embed:"" prefix:"retry."`
}

// Retry configures how the router retries a request on one chain entry
// before it moves to the next. The defaults are the ones the Anthropic and
// OpenAI SDKs ship with (their generated clients share them): 2 retries,
// 0.5s first wait doubling to at most 8s, a wait shortened by up to 25%,
// and a retry-after honoured up to 60s. These were written from memory and
// not re-read from the SDK sources.
type Retry struct {
	// MaxRetries is how many times a request is sent again to the same
	// entry. Zero turns retrying off.
	MaxRetries int `name:"max_retries" default:"2" validate:"gte=0" help:"Times a failed request is sent again to the same chain entry. 0 turns retrying off."`
	// InitialBackoff is the wait before the first retry. Each later wait is
	// Multiplier times the one before, up to MaxBackoff.
	InitialBackoff time.Duration `name:"initial_backoff" default:"500ms" validate:"gt=0" help:"Wait before the first retry."`
	MaxBackoff     time.Duration `name:"max_backoff" default:"8s" validate:"gtefield=InitialBackoff" help:"Longest wait between retries."`
	Multiplier     float64       `name:"multiplier" default:"2" validate:"gte=1,finite" help:"Factor from one wait to the next."`
	// Jitter is the largest fraction a wait is shortened by, chosen at
	// random for each wait, so that many processes do not retry together.
	Jitter float64 `name:"jitter" default:"0.25" validate:"gte=0,lte=1" help:"Largest fraction a wait is shortened by, chosen at random for each wait."`
	// MaxRetryAfter is the longest wait a provider may ask for and still be
	// waited for. A longer one counts as retries exhausted.
	MaxRetryAfter time.Duration `name:"max_retry_after" default:"60s" validate:"gte=0" help:"Longest wait a provider may ask for and still get. A longer one counts as retries exhausted."`
}

func (l Limits) validate() error {
	return validateFields("limits", l)
}

// Validate reports retry settings the router cannot act on.
func (r Retry) Validate() error {
	return validateFields("limits.retry", r)
}
