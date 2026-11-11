package router_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
)

var policy = config.Retry{
	MaxRetries:     3,
	InitialBackoff: time.Second,
	MaxBackoff:     5 * time.Second,
	Multiplier:     2,
	Jitter:         0.5,
	MaxRetryAfter:  time.Minute,
}

func TestBackoffGrowsToTheMaximumAndJitterOnlyShortensIt(t *testing.T) {
	var waits []time.Duration
	for retries := range 5 {
		waits = append(waits, router.Backoff(policy, retries, 0))
	}
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}, waits)

	assert.Equal(t, 3*time.Second, router.Backoff(policy, 2, 0.5), "half the jitter range takes a quarter off")
	// rand is below 1, so the wait stays above the unjittered half.
	assert.Greater(t, router.Backoff(policy, 2, 0.999999), 2*time.Second)
	assert.Equal(t, 5*time.Second, router.Backoff(policy, 4000, 0), "a large retry count does not overflow")
}

func TestDecide(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	overload := &provider.Error{Class: provider.Retry, Reason: provider.ReasonOverload}
	limited := &provider.Error{Class: provider.Retry, Reason: provider.ReasonRateLimit}
	network := &provider.Error{Class: provider.Retry, Reason: provider.ReasonNetwork}
	for _, tc := range []struct {
		name string
		err  *provider.Error
		at   router.Attempt
		want router.Decision
	}{
		{
			"a retryable error waits the backoff",
			overload,
			router.Attempt{Retries: 1, Entries: 2},
			router.Decision{Action: router.Again, Wait: 2 * time.Second},
		},
		{
			"retry-after replaces the backoff and gets no jitter",
			&provider.Error{Class: provider.Retry, Reason: provider.ReasonOverload, RetryAfter: 7 * time.Second},
			router.Attempt{Entries: 2, Rand: 0.9},
			router.Decision{Action: router.Again, Wait: 7 * time.Second},
		},
		{
			"a reset time is waited for when it is later than retry-after",
			&provider.Error{Class: provider.Retry, Reason: provider.ReasonRateLimit, RetryAfter: time.Second, ResetAt: now.Add(20 * time.Second)},
			router.Attempt{Entries: 2, Now: now},
			router.Decision{Action: router.Again, Wait: 20 * time.Second},
		},
		{
			"a reset time in the past falls back to the backoff",
			&provider.Error{Class: provider.Retry, Reason: provider.ReasonRateLimit, ResetAt: now.Add(-time.Hour)},
			router.Attempt{Entries: 2, Now: now},
			router.Decision{Action: router.Again, Wait: time.Second},
		},
		{
			"a wait longer than the policy allows moves on instead",
			&provider.Error{Class: provider.Retry, Reason: provider.ReasonRateLimit, RetryAfter: time.Hour},
			router.Attempt{Entries: 2},
			router.Decision{Action: router.Next},
		},
		{
			"used-up retries move to the next entry",
			overload,
			router.Attempt{Retries: 3, Entries: 2},
			router.Decision{Action: router.Next},
		},
		{
			"used-up retries on the last entry return the error",
			overload,
			router.Attempt{Retries: 3, Entry: 1, Entries: 2},
			router.Decision{Action: router.Return},
		},
		{
			"a network failure is not another account's to fix",
			network,
			router.Attempt{Retries: 3, Entries: 2},
			router.Decision{Action: router.Return},
		},
		{
			"the fallback class moves on without a retry",
			&provider.Error{Class: provider.Fallback, Reason: provider.ReasonAuth},
			router.Attempt{Entries: 2},
			router.Decision{Action: router.Next},
		},
		{
			"the fallback class on the last entry returns the error",
			&provider.Error{Class: provider.Fallback, Reason: provider.ReasonRateLimit},
			router.Attempt{Entry: 1, Entries: 2},
			router.Decision{Action: router.Return},
		},
		{
			"the fail class returns the error",
			&provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest},
			router.Attempt{Entries: 2},
			router.Decision{Action: router.Return},
		},
		{
			"a chain of one does not wait out a rate limit",
			&provider.Error{Class: provider.Retry, Reason: provider.ReasonRateLimit, RetryAfter: time.Second},
			router.Attempt{Entries: 1},
			router.Decision{Action: router.Return},
		},
		{
			"a chain of one still retries an overload",
			overload,
			router.Attempt{Entries: 1},
			router.Decision{Action: router.Again, Wait: time.Second},
		},
		{
			"a rate limit with a longer chain is retried",
			limited,
			router.Attempt{Entries: 2},
			router.Decision{Action: router.Again, Wait: time.Second},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, router.Decide(policy, tc.err, tc.at))
		})
	}
}
