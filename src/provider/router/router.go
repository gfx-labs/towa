// Package router sends a model request down a chain of provider entries:
// it retries an entry with backoff, moves to the next entry when the error
// says another account can help, and reports each move.
package router

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
)

// Entry is one step of a model's chain.
type Entry struct {
	// Provider is the client for the entry's endpoint.
	Provider provider.Provider
	// ProviderID names the provider in the configuration.
	ProviderID string
	// Account is the id of the account the request is sent with.
	Account string
	// Model is the provider's own name for the model.
	Model string
	// Preparation is the selected model's request metadata. Nil keeps the caller's legacy metadata.
	Preparation *Preparation
}

// Preparation is supplied by model wiring for each actual chain entry.
// A non-nil value must have positive context and output allowances.
type Preparation struct {
	ContextWindow    int64
	MaxOutputTokens  int64
	Interruption     string
	InterruptionRole string
	System           []provider.SystemBlock
}

// String returns the entry as a chain names it: provider:account/model.
func (e Entry) String() string {
	return fmt.Sprintf("%s:%s/%s", e.ProviderID, e.Account, e.Model)
}

// MoveKind says what the router did after a failed request.
type MoveKind string

// Kinds of move.
const (
	// MoveRetry: the request is sent to the same entry again after Wait.
	MoveRetry MoveKind = "retry"
	// MoveFallback: the request is sent to the next entry.
	MoveFallback MoveKind = "fallback"
	// MoveRefresh: the credential was refused and a new one was obtained.
	// The request is sent to the same entry with it.
	MoveRefresh MoveKind = "refresh"
)

// Move is one step the router took after a failed request.
type Move struct {
	Kind MoveKind
	// From is the entry that failed, and To the entry the request goes to
	// next. They are the same for a retry and a refresh.
	From, To Entry
	// Wait is how long the router waits before sending.
	Wait time.Duration
	// Err is the failure that caused the move.
	Err *provider.Error
}

type (
	reportKey  struct{}
	prepareKey struct{}
)

// WithPrepare installs the request preparation owned by the turn. It runs
// once for each selected chain entry, before any request is sent there, so
// provider-specific context can be made portable before a fallback. A retry
// on the same entry reuses the prepared request. Its callback context masks
// this hook, so a summary model's router does not recursively prepare itself.
func WithPrepare(ctx context.Context, prepare func(context.Context, Entry, provider.Request) (provider.Request, error)) context.Context {
	return context.WithValue(ctx, prepareKey{}, prepare)
}

// WithReport returns a context whose requests report their moves to report.
// It is called on the goroutine that ranges over the stream, before the
// wait.
func WithReport(ctx context.Context, report func(Move)) context.Context {
	return context.WithValue(ctx, reportKey{}, report)
}

// Failed says which entry a request's error came from. It is in the chain
// of the *provider.Error the router returns, so errors.As finds it.
type Failed struct {
	Entry Entry
	// Err is the entry's own underlying error, when there was one.
	Err error
}

func (f *Failed) Error() string {
	if f.Err == nil {
		return "at " + f.Entry.String()
	}
	return fmt.Sprintf("at %s: %v", f.Entry, f.Err)
}

func (f *Failed) Unwrap() error { return f.Err }

// Options configures a Router. Every field is required.
type Options struct {
	// Chain is tried in order. It has at least one entry.
	Chain []Entry
	Auth  auth.Source
	Retry config.Retry
	// Rand returns a number in [0, 1) for the jitter of one wait.
	Rand func() float64
}

// Router is a provider.Provider over a chain of entries.
type Router struct {
	chain  []Entry
	source auth.Source
	retry  config.Retry
	rand   func() float64
}

var _ provider.Provider = (*Router)(nil)

// New returns a router over the chain.
func New(o Options) (*Router, error) {
	if len(o.Chain) == 0 {
		return nil, errors.New("router: the chain has no entries")
	}
	for i, e := range o.Chain {
		if e.Provider == nil || e.Account == "" || e.Model == "" {
			return nil, fmt.Errorf("router: chain entry %d (%s) needs a provider, an account and a model", i, e)
		}
		if e.Preparation != nil && (e.Preparation.ContextWindow <= 0 || e.Preparation.MaxOutputTokens <= 0) {
			return nil, fmt.Errorf("router: chain entry %d (%s) needs positive context and output allowances", i, e)
		}
	}
	if o.Auth == nil || o.Rand == nil {
		return nil, errors.New("router: Auth and Rand are required")
	}
	if err := o.Retry.Validate(); err != nil {
		return nil, fmt.Errorf("router: %w", err)
	}
	return &Router{chain: o.Chain, source: o.Auth, retry: o.Retry, rand: o.Rand}, nil
}

// Stream sends req to the first entry of the chain, with that entry's model
// and credential in place of the request's, and moves down the chain as
// Decide says.
//
// A request is sent again only while its response has yielded nothing. Once
// an event has reached the caller the client may have been shown it, so a
// failure after that is returned as it is.
func (r *Router) Stream(ctx context.Context, req provider.Request) iter.Seq2[provider.Event, error] {
	return func(yield func(provider.Event, error) bool) {
		report, _ := ctx.Value(reportKey{}).(func(Move))
		if report == nil {
			report = func(Move) {}
		}
		for i := 0; i < len(r.chain); i++ {
			entry := r.chain[i]
			cred, err := r.source.Credential(ctx, entry.Account)
			if err != nil {
				// No credential is the same to the caller as a refused one.
				failure := &provider.Error{Class: provider.Fallback, Reason: provider.ReasonAuth, Err: err}
				if i < len(r.chain)-1 && ctx.Err() == nil {
					report(Move{Kind: MoveFallback, From: entry, To: r.chain[i+1], Err: failure})
					continue
				}
				yield(provider.Event{}, at(entry, failure))
				return
			}
			if prepare, ok := ctx.Value(prepareKey{}).(func(context.Context, Entry, provider.Request) (provider.Request, error)); ok {
				req.Model, req.Credential = entry.Model, cred
				prepared, err := prepare(context.WithValue(ctx, prepareKey{}, struct{}{}), entry, req)
				if err != nil {
					failure := &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Err: err}
					var perr *provider.Error
					if errors.As(err, &perr) {
						failureCopy := *perr
						failureCopy.Err = err
						failure = &failureCopy
					}
					yield(provider.Event{}, at(entry, failure))
					return
				}
				req = prepared
			}
			// A summary router has no prepare hook and keeps its own prompt,
			// but the actual endpoint's output allowance still applies.
			if selected := entry.Preparation; selected != nil && (req.MaxOutputTokens == 0 || req.MaxOutputTokens > selected.MaxOutputTokens) {
				req.MaxOutputTokens = selected.MaxOutputTokens
			}
			refreshed := false
			for retries := 0; ; {
				req.Model, req.Credential = entry.Model, cred
				failure, stop := r.attempt(ctx, entry, req, yield)
				if stop {
					return
				}
				if failure.Reason == provider.ReasonAuth && !refreshed {
					// One refresh per entry: a second refusal is the
					// account's, not the token's age.
					refreshed = true
					if fresh, err := r.source.Refresh(ctx, cred); err == nil {
						cred = fresh
						report(Move{Kind: MoveRefresh, From: entry, To: entry, Err: failure})
						continue
					}
				}
				decision := Decide(r.retry, failure, Attempt{
					Retries: retries, Entry: i, Entries: len(r.chain), Now: time.Now(), Rand: r.rand(),
				})
				if decision.Action == Again {
					report(Move{Kind: MoveRetry, From: entry, To: entry, Wait: decision.Wait, Err: failure})
					if err := sleep(ctx, decision.Wait); err != nil {
						yield(provider.Event{}, at(entry, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Err: err}))
						return
					}
					retries++
					continue
				}
				if decision.Action == Next {
					report(Move{Kind: MoveFallback, From: entry, To: r.chain[i+1], Err: failure})
					break
				}
				yield(provider.Event{}, at(entry, failure))
				return
			}
		}
	}
}

// attempt sends one request to one entry. It returns the failure when the
// request can still be sent again, and stop when the stream is over: it
// ended, the caller stopped ranging, or it failed after yielding.
func (r *Router) attempt(ctx context.Context, entry Entry, req provider.Request, yield func(provider.Event, error) bool) (failure *provider.Error, stop bool) {
	yielded := false
	for event, err := range entry.Provider.Stream(ctx, req) {
		if err != nil {
			var perr *provider.Error
			if !errors.As(err, &perr) {
				// The contract says every error is a *provider.Error. One
				// that is not cannot be classified, so it ends the turn.
				perr = &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Err: err}
			}
			if yielded || ctx.Err() != nil {
				yield(provider.Event{}, at(entry, perr))
				return nil, true
			}
			return perr, false
		}
		if event.Usage != nil && event.Usage.Account == "" {
			// A provider holds no account, so it cannot name it.
			usage := *event.Usage
			usage.Account = entry.Account
			event.Usage = &usage
		}
		yielded = true
		if !yield(event, nil) {
			return nil, true
		}
	}
	if !yielded {
		yield(provider.Event{}, at(entry, &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "the response ended without an event",
		}))
	}
	return nil, true
}

// at returns e with the entry it came from in its chain.
func at(entry Entry, e *provider.Error) *provider.Error {
	out := *e
	out.Err = &Failed{Entry: entry, Err: e.Err}
	return &out
}

// sleep waits for d or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
