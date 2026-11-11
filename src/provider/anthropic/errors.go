package anthropic

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gfx-labs/acpharness/src/provider"
)

// apiError is the error object of an error body or of an error event.
type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// parseErrorObject reads an error object. A value that is not one yields
// what could be read, which may be nothing.
func parseErrorObject(raw jsontext.Value) apiError {
	var out apiError
	if len(raw) == 0 {
		return out
	}
	if raw.Kind() == '"' {
		_ = json.Unmarshal(raw, &out.Message)
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return apiError{}
	}
	return out
}

// parseErrorBody reads the body of an HTTP error response. A body that is
// not the API's error JSON, such as a gateway's page, becomes the message
// as it is.
func parseErrorBody(data []byte) apiError {
	var body struct {
		Error jsontext.Value `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err == nil {
		if out := parseErrorObject(body.Error); out != (apiError{}) {
			return out
		}
	}
	return apiError{Message: strings.TrimSpace(string(data))}
}

// contextFullPhrases are how the API words a request that does not fit the
// model. It has no error type or code for it: both arrive as
// invalid_request_error with HTTP 400, so the message is the only signal.
// The phrases are the ones jcode and Claude Code match. They are not yet
// checked against a recording.
var contextFullPhrases = []string{
	"prompt is too long",
	"exceed context limit",
	"exceeds the context window",
	"context window",
}

// quotaPhrases mark an account that is out of paid usage, which the API
// reports as an invalid request.
var quotaPhrases = []string{
	"credit balance is too low",
}

func containsAny(s string, phrases []string) bool {
	for _, p := range phrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// classify turns a failed response or an error event into an error. status
// is zero and header is nil for an error event.
func classify(status int, header http.Header, e apiError) *provider.Error {
	out := &provider.Error{Status: status, Message: e.Message}
	if out.Message == "" {
		out.Message = e.Type
	}
	lower := strings.ToLower(e.Message)
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden ||
		e.Type == "authentication_error" || e.Type == "permission_error":
		// Fallback, not Fail: the caller refreshes the credential once and
		// then moves to another account.
		out.Class, out.Reason = provider.Fallback, provider.ReasonAuth
	case status == http.StatusTooManyRequests || e.Type == "rate_limit_error":
		out.Class, out.Reason = provider.Retry, provider.ReasonRateLimit
		out.RetryAfter = retryAfter(header)
		out.ResetAt = resetAt(header)
	case status == http.StatusPaymentRequired || e.Type == "billing_error" || containsAny(lower, quotaPhrases):
		// Waiting does not refill an account. Another account can answer.
		out.Class, out.Reason = provider.Fallback, provider.ReasonRateLimit
	case status == statusOverloaded || e.Type == "overloaded_error":
		out.Class, out.Reason = provider.Retry, provider.ReasonOverload
		out.RetryAfter = retryAfter(header)
	case (status == http.StatusBadRequest || e.Type == "invalid_request_error") && containsAny(lower, contextFullPhrases):
		out.Class, out.Reason = provider.Fail, provider.ReasonContextFull
	case status >= 500 || e.Type == "api_error" || e.Type == "timeout_error":
		out.Class, out.Reason = provider.Retry, provider.ReasonOverload
		out.RetryAfter = retryAfter(header)
	case status == http.StatusRequestTimeout:
		out.Class, out.Reason = provider.Retry, provider.ReasonNetwork
	default:
		// invalid_request_error, not_found_error, request_too_large and
		// anything not known: the same request would fail again.
		out.Class, out.Reason = provider.Fail, provider.ReasonRequest
	}
	return out
}

// statusOverloaded is the status Anthropic answers with when it is
// overloaded. net/http has no name for it.
const statusOverloaded = 529

// retryAfter reads the wait from the response headers. It is zero when they
// name none.
func retryAfter(h http.Header) time.Duration {
	value := h.Get("Retry-After")
	if secs, err := strconv.ParseFloat(value, 64); err == nil && secs > 0 {
		return time.Duration(secs * float64(time.Second))
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}

// apiKeyLimits are the limits an API key response reports, each with an
// anthropic-ratelimit-<name>-remaining count and an RFC 3339
// anthropic-ratelimit-<name>-reset time.
var apiKeyLimits = []string{"requests", "tokens", "input-tokens", "output-tokens"}

// resetAt reads when the limit that refused the request resets. It is the
// zero time when the headers do not say.
func resetAt(h http.Header) time.Time {
	// A subscription reports one unified limit, as Unix seconds.
	if secs, err := strconv.ParseInt(h.Get("Anthropic-Ratelimit-Unified-Reset"), 10, 64); err == nil && secs > 0 {
		return time.Unix(secs, 0)
	}
	// An API key has several limits. The ones with nothing remaining
	// refused the request, and the last of them to reset decides.
	var latest time.Time
	for _, name := range apiKeyLimits {
		if h.Get("Anthropic-Ratelimit-"+name+"-Remaining") != "0" {
			continue
		}
		at, err := time.Parse(time.RFC3339, h.Get("Anthropic-Ratelimit-"+name+"-Reset"))
		if err == nil && at.After(latest) {
			latest = at
		}
	}
	return latest
}

// transportError is a request that did not reach the API or a response that
// was cut off. A cancelled context is not retried.
func transportError(ctx context.Context, what string, err error) *provider.Error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: what, Err: ctxErr}
	}
	return &provider.Error{Class: provider.Retry, Reason: provider.ReasonNetwork, Message: what, Err: err}
}
