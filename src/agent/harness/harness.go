// Package harness defines what towa adds to ACP messages under the
// "towa" key of _meta, and under the same key in the data of an error
// response. A client reads the additions in that shape.
package harness

import (
	"encoding/json/v2"
	"fmt"
	"time"

	acp "github.com/gfx-labs/go-acp"
)

// Key is the name towa uses in _meta and in error data.
const Key = "towa"

// StopKind says why a turn stopped, from a fixed list. A client decides what
// to do from it and never from the text of an error.
type StopKind string

// Kinds of stop.
const (
	// StopRateLimit: the account is rate limited or out of quota, and every
	// entry of the model's chain was tried.
	StopRateLimit StopKind = "rate_limit"
	// StopAuth: the provider rejected the account's credential.
	StopAuth StopKind = "auth"
	// StopOverload: the provider is overloaded or unavailable.
	StopOverload StopKind = "overload"
	// StopContextFull: the conversation does not fit the model's context
	// and compaction is off or could not make room.
	StopContextFull StopKind = "context_full"
	// StopLimit: a configured limit was reached. Limit names it.
	StopLimit StopKind = "limit"
	// StopNetwork: the provider could not be reached.
	StopNetwork StopKind = "network"
	// StopRequest: the provider rejected the request as invalid.
	StopRequest StopKind = "request"
	// StopDenied: a configured prompt hook refused the prompt, so the turn
	// did not start. Message is the hook's reason.
	StopDenied StopKind = "denied"
)

// Stop is why a turn stopped short of the model ending it.
//
// A turn that ends with a stop reason carries it in the prompt response's
// _meta. A turn that fails carries it in the data of the error response.
type Stop struct {
	Kind StopKind `json:"kind"`
	// ResetAt is when the provider said the rate limit resets.
	ResetAt time.Time `json:"resetAt,omitzero"`
	// RetryAfterSeconds is how long the provider asked the caller to wait.
	RetryAfterSeconds float64 `json:"retryAfterSeconds,omitzero"`
	// Limit is the config key of the limit that was reached.
	Limit string `json:"limit,omitzero"`
	// Account and Model say which chain entry reported it.
	Account string `json:"account,omitzero"`
	Model   string `json:"model,omitzero"`
	// Message is the provider's own text, or a hook's reason, for people.
	Message string `json:"message,omitzero"`
}

// MoveKind says what the harness did about a failed model request.
type MoveKind string

// Kinds of move.
const (
	// MoveRetry: the same request is sent again to the same account.
	MoveRetry MoveKind = "retry"
	// MoveFallback: the request goes to the next entry of the model's chain.
	MoveFallback MoveKind = "fallback"
)

// Move reports one retry or fallback while a turn runs. It is carried in
// the _meta of a session_info_update that sets nothing else, which every
// client accepts.
type Move struct {
	Kind MoveKind `json:"kind"`
	// Reason is why the request failed, from the same list a Stop uses.
	Reason StopKind `json:"reason"`
	// FromAccount and FromModel say which chain entry failed.
	FromAccount string `json:"fromAccount,omitzero"`
	FromModel   string `json:"fromModel,omitzero"`
	// ToAccount and ToModel say which entry is tried next. For a retry they
	// are the same as the From fields.
	ToAccount string `json:"toAccount,omitzero"`
	ToModel   string `json:"toModel,omitzero"`
	// WaitSeconds is how long the harness waits before it sends again.
	WaitSeconds float64 `json:"waitSeconds,omitzero"`
}

// Addition is what towa puts under Key. At most one field is set.
type Addition struct {
	Stop *Stop `json:"stop,omitzero"`
	Move *Move `json:"move,omitzero"`
}

// Meta returns meta with a under Key. meta may be nil.
func Meta(meta map[string]any, a Addition) map[string]any {
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out[Key] = a
	return out
}

// FromMeta reads the addition in a message's _meta. It reports false when
// there is none.
func FromMeta(meta map[string]any) (Addition, bool, error) {
	return decode(meta)
}

// FromError reads the addition in the data of an error response. It reports
// false when there is none.
func FromError(e *acp.RequestError) (Addition, bool, error) {
	data, ok := e.Data.(map[string]any)
	if !ok {
		return Addition{}, false, nil
	}
	return decode(data)
}

func decode(from map[string]any) (Addition, bool, error) {
	value, ok := from[Key]
	if !ok {
		return Addition{}, false, nil
	}
	if a, ok := value.(Addition); ok {
		return a, true, nil
	}
	// The value came off the wire as untyped JSON.
	raw, err := json.Marshal(value)
	if err != nil {
		return Addition{}, false, fmt.Errorf("harness: %w", err)
	}
	var a Addition
	if err := json.Unmarshal(raw, &a); err != nil {
		return Addition{}, false, fmt.Errorf("harness: %w", err)
	}
	return a, true, nil
}
