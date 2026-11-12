package provider

import (
	"context"
	"errors"
	"time"

	"github.com/gfx-labs/towa/src/auth"
)

// Usage is how much of a subscription's limits an account has used.
type Usage struct {
	// Plan is the provider's name for the subscription. Empty when the
	// provider does not say.
	Plan string
	// Limits are the limits the provider reports, in its order.
	Limits []UsageLimit
}

// UsageLimit is one limit of a subscription.
type UsageLimit struct {
	// Name says which limit this is, in the provider's own words where it
	// has them: "seven_day", or "7d" for a window given by its length.
	Name string
	// UsedPercent is how much of the limit is used, from 0 to 100.
	UsedPercent float64
	// ResetsAt is when the limit resets. It is the zero time when the
	// provider does not say.
	ResetsAt time.Time
}

// ErrNoUsage is what Usage returns for a credential whose endpoint reports
// no usage: only a subscription login has limits to report.
var ErrNoUsage = errors.New("the endpoint reports no subscription usage for this kind of account")

// UsageReporter is a client whose endpoint reports an account's
// subscription usage.
type UsageReporter interface {
	Usage(ctx context.Context, cred auth.Credential) (Usage, error)
}
