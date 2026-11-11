package loop

import (
	"errors"

	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
)

// StopKind returns the stop kind a client sees for a provider's reason. It
// reports false for a reason that is not in provider's list.
func StopKind(reason provider.Reason) (harness.StopKind, bool) {
	switch reason {
	case provider.ReasonRateLimit:
		return harness.StopRateLimit, true
	case provider.ReasonAuth:
		return harness.StopAuth, true
	case provider.ReasonOverload:
		return harness.StopOverload, true
	case provider.ReasonContextFull:
		return harness.StopContextFull, true
	case provider.ReasonNetwork:
		return harness.StopNetwork, true
	case provider.ReasonRequest:
		return harness.StopRequest, true
	}
	return "", false
}

// StopFor turns a failed model request into the error a turn returns. An
// error with a *provider.Error in its chain becomes a *StopError with the
// provider's reset time, retry-after and message, and the account and model
// of the chain entry that failed when the router named it. Any other error,
// and a provider error with a reason outside the list, is returned as it is.
func StopFor(err error) error {
	var perr *provider.Error
	if !errors.As(err, &perr) {
		return err
	}
	kind, ok := StopKind(perr.Reason)
	if !ok {
		return err
	}
	stop := harness.Stop{
		Kind:              kind,
		ResetAt:           perr.ResetAt,
		RetryAfterSeconds: perr.RetryAfter.Seconds(),
		Message:           perr.Message,
	}
	var failed *router.Failed
	if errors.As(err, &failed) {
		stop.Account, stop.Model = failed.Entry.Account, failed.Entry.Model
	}
	return &StopError{Stop: stop, Err: err}
}

// MoveFor is the report a client gets for a move of the router. It reports
// false when the failure's reason is outside provider's list.
func MoveFor(m router.Move) (harness.Move, bool) {
	reason, ok := StopKind(m.Err.Reason)
	if !ok {
		return harness.Move{}, false
	}
	kind := harness.MoveRetry
	if m.Kind == router.MoveFallback {
		kind = harness.MoveFallback
	}
	return harness.Move{
		Kind:        kind,
		Reason:      reason,
		FromAccount: m.From.Account,
		FromModel:   m.From.Model,
		ToAccount:   m.To.Account,
		ToModel:     m.To.Model,
		WaitSeconds: m.Wait.Seconds(),
	}, true
}
