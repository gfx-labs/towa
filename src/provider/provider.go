// Package provider is the interface between the agent loop and a model API.
// The wire clients are in its subpackages.
package provider

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"iter"
	"time"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Provider sends model requests to one endpoint. It holds no account: the
// caller picks the account and passes its credential with each request.
type Provider interface {
	// Stream sends one request and yields what the model produces. The
	// sequence ends with an event whose Stop is set, or with an error. Every
	// error is an *Error. Nothing is sent until the sequence is ranged over,
	// and breaking out of the range closes the connection.
	Stream(ctx context.Context, req Request) iter.Seq2[Event, error]
}

// Request is one model request.
type Request struct {
	// Model is the provider's own name for the model.
	Model      string
	Credential auth.Credential
	// System holds the system prompt's sections in order. A client adds
	// what its credential's scheme requires, such as the Claude OAuth
	// billing block, and decides where cache breakpoints go.
	System []SystemBlock
	// Items is the conversation. It holds only User, Text, Thinking,
	// ToolCall, ToolResult and Native items. A client reports any other as
	// an error of class Fail.
	Items []transcript.Item
	Tools []Tool
	// MaxOutputTokens is not sent when zero. A provider whose API requires
	// it reports zero as an error of class Fail, so the caller always sets
	// it from the model's metadata.
	MaxOutputTokens int64
	// Effort is the reasoning effort in the provider's own words. It is not
	// sent when empty.
	Effort string
	// CacheKey identifies the conversation to the provider's prompt cache.
	// The session id is used.
	CacheKey string
}

// SystemBlock is one named section of the system prompt.
type SystemBlock struct {
	Name string
	Text string
}

// Tool describes a tool to the model.
type Tool struct {
	Name        string
	Description string
	// InputSchema is the JSON Schema of the tool's argument.
	InputSchema jsontext.Value
	// Native, when set, is sent as the tool's definition unchanged and the
	// other fields are not used. It is for a provider's built-in tools and
	// for tool types only one provider has, such as a free-text tool with a
	// grammar.
	Native jsontext.Value
}

// Event is one step of a response. Exactly one field is set.
type Event struct {
	// TextDelta and ThinkingDelta are text as it arrives, for showing to the
	// client. The finished text follows as an Item.
	TextDelta     string
	ThinkingDelta string
	// ToolCall reports a tool call the model is still writing.
	ToolCall *ToolCallProgress
	// Item is a finished item, in the order it belongs in the transcript.
	// The loop stores items and ignores deltas.
	Item *transcript.Item
	// Usage may come more than once. The last one is the request's total.
	Usage *transcript.Usage
	// Stop is set on the last event.
	Stop StopReason
}

// ToolCallProgress says the model has started a tool call or written more
// of its argument.
type ToolCallProgress struct {
	ID   string
	Name string
	// InputDelta is the next piece of the argument's text.
	InputDelta string
}

// StopReason says why the model stopped writing.
type StopReason string

// Reasons a response ends.
const (
	StopEndTurn   StopReason = "end_turn"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefusal   StopReason = "refusal"
	// StopPaused: the provider paused a long server-side tool run and the
	// response is not finished. The caller sends the conversation back as
	// it is, with no new user message, and the provider carries on.
	StopPaused StopReason = "paused"
)

// Class says what the caller does about an error.
type Class int

// Classes of an error.
const (
	// Fail ends the turn.
	Fail Class = iota
	// Retry sends the same request to the same account again, after
	// RetryAfter when it is set and after a backoff otherwise.
	Retry
	// Fallback moves to the next entry of the model's chain.
	Fallback
)

// String returns the class as it appears in logs and telemetry.
func (c Class) String() string {
	switch c {
	case Fail:
		return "fail"
	case Retry:
		return "retry"
	case Fallback:
		return "fallback"
	default:
		return fmt.Sprintf("class(%d)", int(c))
	}
}

// Reason is why a request failed, from a fixed list. A client of acpharness
// reads it to decide what to do, so a new reason is a protocol change.
type Reason string

// Reasons a request fails.
const (
	// ReasonRateLimit: the account is rate limited or out of quota. ResetAt
	// or RetryAfter is set when the provider said when it ends.
	ReasonRateLimit Reason = "rate_limit"
	// ReasonAuth: the credential was rejected.
	ReasonAuth Reason = "auth"
	// ReasonOverload: the provider is overloaded or unavailable.
	ReasonOverload Reason = "overload"
	// ReasonContextFull: the request does not fit the model's context.
	ReasonContextFull Reason = "context_full"
	// ReasonNetwork: the request did not reach the provider, or the
	// response was cut off.
	ReasonNetwork Reason = "network"
	// ReasonRequest: the provider rejected the request as invalid. Sending
	// it again cannot help.
	ReasonRequest Reason = "request"
)

// Error is a failed request.
type Error struct {
	Class  Class
	Reason Reason
	// Status is the HTTP status, or zero when there was no response.
	Status int
	// RetryAfter is how long the provider asked the caller to wait. It is
	// zero when the provider did not say.
	RetryAfter time.Duration
	// ResetAt is when the provider said the limit resets. It is the zero
	// time when the provider did not say.
	ResetAt time.Time
	// Message is the provider's own description of the error.
	Message string
	// Err is the underlying error, when there is one.
	Err error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("provider: %s (%s)", e.Reason, e.Class)
	if e.Status != 0 {
		s += fmt.Sprintf(": HTTP %d", e.Status)
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }
