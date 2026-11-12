package router

import (
	"math"
	"time"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
)

// Action is what the router does about a failed request.
type Action int

// Actions after a failed request.
const (
	// Return gives the error to the caller.
	Return Action = iota
	// Again sends the request to the same entry after Decision.Wait.
	Again
	// Next sends the request to the next entry of the chain.
	Next
)

// Decision is an Action and, for Again, how long to wait first.
type Decision struct {
	Action Action
	Wait   time.Duration
}

// Attempt is where a failed request stands in the chain.
type Attempt struct {
	// Retries is how many times the request was already sent again to this
	// entry.
	Retries int
	// Entry is the position of the entry in the chain, from zero, and
	// Entries is the chain's length.
	Entry, Entries int
	// Now is the time of the failure. A reset time is measured from it.
	Now time.Time
	// Rand is a number in [0, 1) that sets the jitter of this wait.
	Rand float64
}

// Decide says what to do about a failed request. It is the whole retry and
// fallback policy.
func Decide(policy config.Retry, e *provider.Error, at Attempt) Decision {
	last := at.Entry == at.Entries-1
	// With one entry there is nowhere to move, and whoever owns the choice
	// of account decides whether to wait. So a rate limit is not waited out.
	if e.Reason == provider.ReasonRateLimit && at.Entries == 1 {
		return Decision{Action: Return}
	}
	switch e.Class {
	case provider.Retry:
		if wait, ok := Wait(policy, e, at); ok {
			return Decision{Action: Again, Wait: wait}
		}
		// Retries are used up. Another account or provider can still help
		// with these reasons, and with no other.
		if !last && movesOn(e.Reason) {
			return Decision{Action: Next}
		}
		return Decision{Action: Return}
	case provider.Fallback:
		if !last {
			return Decision{Action: Next}
		}
		return Decision{Action: Return}
	default:
		return Decision{Action: Return}
	}
}

// movesOn reports whether a request that failed for reason is worth sending
// to the next entry once retries are used up.
func movesOn(reason provider.Reason) bool {
	switch reason {
	case provider.ReasonRateLimit, provider.ReasonAuth, provider.ReasonOverload:
		return true
	default:
		return false
	}
}

// Wait returns how long to wait before sending the request to the same
// entry again. It reports false when the entry's retries are used up: the
// count is reached, or the provider asked for a longer wait than the policy
// allows.
func Wait(policy config.Retry, e *provider.Error, at Attempt) (time.Duration, bool) {
	if at.Retries >= policy.MaxRetries {
		return 0, false
	}
	asked := e.RetryAfter
	if !e.ResetAt.IsZero() {
		asked = max(asked, e.ResetAt.Sub(at.Now))
	}
	if asked > 0 {
		// The provider's own figure is used as it is, without jitter:
		// sending earlier than asked fails again.
		if asked > policy.MaxRetryAfter {
			return 0, false
		}
		return asked, true
	}
	return Backoff(policy, at.Retries, at.Rand), true
}

// Backoff returns the wait before retry number retries, counting from zero:
// the initial backoff times the multiplier once per earlier retry, at most
// the maximum, shortened by up to the jitter fraction. rand is in [0, 1).
func Backoff(policy config.Retry, retries int, rand float64) time.Duration {
	wait := float64(policy.InitialBackoff) * math.Pow(policy.Multiplier, float64(retries))
	// The comparison is done in float64 so that a large exponent cannot
	// overflow the duration.
	if wait > float64(policy.MaxBackoff) {
		wait = float64(policy.MaxBackoff)
	}
	return time.Duration(wait * (1 - policy.Jitter*rand))
}
