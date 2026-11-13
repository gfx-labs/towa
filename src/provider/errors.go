package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// TransportError is a request that did not reach the provider or a response
// that was cut off. A cancelled request is not retried: sending again is the
// caller's decision, not a retry. what names the step and may be empty.
func TransportError(ctx context.Context, what string, err error) *Error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &Error{Class: Fail, Reason: ReasonNetwork, Message: what, Err: ctxErr}
	}
	return &Error{Class: Retry, Reason: ReasonNetwork, Message: what, Err: err}
}

// ParseRetryAfter reads a Retry-After value: a number of seconds or an HTTP
// date. ok is false for a value that is neither, and for an empty one. A date
// that has passed gives a zero wait.
func ParseRetryAfter(value string) (wait time.Duration, ok bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second)), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0), true
	}
	return 0, false
}

// RetryAfter reads the wait from the Retry-After response header. It is zero
// when the header names none.
func RetryAfter(h http.Header) time.Duration {
	wait, _ := ParseRetryAfter(h.Get("Retry-After"))
	return wait
}

// StatusError reads the body of a response whose status is not 200, up to
// limit bytes, and returns what classify makes of it. A failed read is a
// transport error that carries the status.
func StatusError(ctx context.Context, resp *http.Response, limit int64, classify func(body []byte) *Error) *Error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		perr := TransportError(ctx, "read error response", err)
		perr.Status = resp.StatusCode
		return perr
	}
	over := int64(len(data)) > limit
	if over {
		data = data[:limit]
	}
	perr := classify(data)
	if over {
		perr.Message += fmt.Sprintf(" (the error response is longer than %d bytes and was not read to its end)", limit)
	}
	return perr
}
