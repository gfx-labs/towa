package loop

import (
	"errors"

	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/models"
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
// of the chain entry that failed when the router named it. A credential the
// auth source could not give, such as a login that has to be redone, comes
// from the router as an auth failure and so becomes an auth stop. Any other error,
// and a provider error with a reason outside the list, is returned as it is.
// A selection without a configured model is a request stop, not a provider failure.
func StopFor(err error) error {
	var stopped *StopError
	if errors.As(err, &stopped) {
		return stopped
	}
	var noModel *models.NoModelError
	if errors.As(err, &noModel) {
		return &StopError{Stop: harness.Stop{Kind: harness.StopRequest, Message: noModel.Error()}, Err: err}
	}
	var perr *provider.Error
	if !errors.As(err, &perr) {
		var login *auth.LoginRequiredError
		if errors.As(err, &login) {
			return &StopError{Stop: harness.Stop{Kind: harness.StopAuth, Account: login.Account, Message: login.Error()}, Err: err}
		}
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
	// A dead login never reached the provider, so the provider said
	// nothing. The auth source's text tells the user which login to redo.
	var login *auth.LoginRequiredError
	if errors.As(err, &login) && stop.Message == "" {
		stop.Message = login.Error()
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
