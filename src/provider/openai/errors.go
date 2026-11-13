package openai

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gfx-labs/towa/src/provider"
)

// apiError is the error object of a failed response, an error event or an
// error body.
type apiError struct {
	Type    string
	Code    string
	Message string
	// ResetsAt and ResetsIn come with a usage limit. They are zero when the
	// provider did not send them.
	ResetsAt time.Time
	ResetsIn time.Duration
}

// errorObject reads the fields leniently: resets_at has been seen as a
// number and as a string, and code is a number on some gateways.
type errorObject struct {
	Type            string         `json:"type"`
	Code            jsontext.Value `json:"code"`
	Message         string         `json:"message"`
	ResetsAt        jsontext.Value `json:"resets_at"`
	ResetsInSeconds jsontext.Value `json:"resets_in_seconds"`
}

func scalarText(v jsontext.Value) string {
	var s string
	if len(v) == 0 || v.Kind() == 'n' {
		return ""
	}
	if v.Kind() == '"' {
		if json.Unmarshal(v, &s) != nil {
			return ""
		}
		return s
	}
	return strings.TrimSpace(string(v))
}

func scalarInt(v jsontext.Value) (int64, bool) {
	n, err := strconv.ParseInt(scalarText(v), 10, 64)
	return n, err == nil
}

// parseErrorObject reads an error object. A value that is not one yields an
// apiError with what could be read, which may be nothing.
func parseErrorObject(raw jsontext.Value) apiError {
	if len(raw) == 0 {
		return apiError{}
	}
	if raw.Kind() == '"' {
		return apiError{Message: scalarText(raw)}
	}
	var obj errorObject
	if err := json.Unmarshal(raw, &obj); err != nil {
		return apiError{}
	}
	out := apiError{Type: obj.Type, Code: scalarText(obj.Code), Message: obj.Message}
	if at, ok := scalarInt(obj.ResetsAt); ok && at > 0 {
		out.ResetsAt = time.Unix(at, 0)
	}
	if in, ok := scalarInt(obj.ResetsInSeconds); ok && in >= 0 {
		out.ResetsIn = time.Duration(in) * time.Second
	}
	return out
}

// parseErrorBody reads the body of an HTTP error response. A body that is
// not the API's error JSON becomes the message as it is.
func parseErrorBody(data []byte) apiError {
	var body struct {
		Error  jsontext.Value `json:"error"`
		Detail jsontext.Value `json:"detail"`
	}
	if err := json.Unmarshal(data, &body); err == nil {
		if out := parseErrorObject(body.Error); out != (apiError{}) {
			return out
		}
		// The ChatGPT backend answers some failures with {"detail": ...}.
		if detail := scalarText(body.Detail); detail != "" {
			return apiError{Message: detail}
		}
	}
	return apiError{Message: strings.TrimSpace(string(data))}
}

// retryDelayPattern finds the wait a rate limit message names. openai/codex
// ce0323f2 reads the same phrase, because the API sends no header with a
// failed stream event.
var retryDelayPattern = regexp.MustCompile(`(?i)try again in\s*(\d+(?:\.\d+)?)\s*(ms|s|seconds?)`)

func retryDelayFromMessage(msg string) time.Duration {
	m := retryDelayPattern.FindStringSubmatch(msg)
	if m == nil {
		return 0
	}
	value, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	if strings.EqualFold(m[2], "ms") {
		return time.Duration(value * float64(time.Millisecond))
	}
	return time.Duration(value * float64(time.Second))
}

// retryAfter reads the wait from the response headers. OpenAI sends
// retry-after-ms, which is more exact than retry-after, so it comes first.
// It is zero when they name none.
func retryAfter(h http.Header) time.Duration {
	if ms, err := strconv.ParseFloat(h.Get("retry-after-ms"), 64); err == nil && ms > 0 {
		return time.Duration(ms * float64(time.Millisecond))
	}
	return provider.RetryAfter(h)
}

// contextPhrases mark a context-length error from an endpoint that sends no
// error code for it. They are matched in the provider's own message.
var contextPhrases = []string{"context_length_exceeded", "context window", "maximum context length"}

// classify turns a failure into an error the loop can act on. status is
// zero for a failure reported inside a stream.
func classify(status int, header http.Header, e apiError) *provider.Error {
	out := &provider.Error{Status: status, Message: e.Message, RetryAfter: retryAfter(header)}
	if out.Message == "" {
		out.Message = strings.TrimSpace(e.Type + " " + e.Code)
	}
	kind := e.Code
	if kind == "" {
		kind = e.Type
	}
	// The ChatGPT backend puts the kind in type and the API puts it in
	// code, so both are looked at.
	switch {
	case kind == "context_length_exceeded" || e.Type == "context_length_exceeded":
		out.Class, out.Reason = provider.Fail, provider.ReasonContextFull
	case e.Type == "usage_limit_reached" || kind == "usage_limit_reached":
		// A plan's usage window is used up. Waiting on this account takes
		// hours, so the chain moves on.
		out.Class, out.Reason = provider.Fallback, provider.ReasonRateLimit
		out.ResetAt = e.ResetsAt
		if out.ResetAt.IsZero() && e.ResetsIn > 0 {
			out.ResetAt = time.Now().Add(e.ResetsIn)
		}
	case isQuota(kind) || isQuota(e.Type):
		out.Class, out.Reason = provider.Fallback, provider.ReasonRateLimit
	case kind == "rate_limit_exceeded" || kind == "slow_down":
		out.Class, out.Reason = provider.Retry, provider.ReasonRateLimit
		if out.RetryAfter == 0 {
			out.RetryAfter = retryDelayFromMessage(e.Message)
		}
	case kind == "server_is_overloaded":
		out.Class, out.Reason = provider.Retry, provider.ReasonOverload
	case kind == "websocket_connection_limit_reached" || kind == "previous_response_not_found":
		// Both are cured by a new connection that sends the whole input.
		out.Class, out.Reason = provider.Retry, provider.ReasonNetwork
	case kind == "invalid_api_key" || kind == "token_expired" || kind == "token_revoked" || kind == "invalid_token":
		out.Class, out.Reason = provider.Fallback, provider.ReasonAuth
	case kind == "invalid_prompt" || kind == "cyber_policy" || kind == "bio_policy" || kind == "misalignment_policy_violation":
		out.Class, out.Reason = provider.Fail, provider.ReasonRequest
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		out.Class, out.Reason = provider.Fallback, provider.ReasonAuth
	case status == http.StatusTooManyRequests:
		out.Class, out.Reason = provider.Retry, provider.ReasonRateLimit
		if out.RetryAfter == 0 {
			out.RetryAfter = retryDelayFromMessage(e.Message)
		}
	case mentionsContext(e.Message):
		out.Class, out.Reason = provider.Fail, provider.ReasonContextFull
	case status == http.StatusRequestTimeout || status >= 500:
		out.Class, out.Reason = provider.Retry, provider.ReasonOverload
	case status == 0:
		// A failed response with a code this client does not know.
		// openai/codex ce0323f2 retries these.
		out.Class, out.Reason = provider.Retry, provider.ReasonOverload
	default:
		out.Class, out.Reason = provider.Fail, provider.ReasonRequest
	}
	if out.Message == "" {
		out.Message = fmt.Sprintf("the provider gave no description (status %d)", status)
	}
	return out
}

func isQuota(kind string) bool {
	switch kind {
	case "insufficient_quota", "usage_not_included", "credit_balance_exhausted",
		"organization_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded":
		return true
	}
	return false
}

func mentionsContext(msg string) bool {
	lower := strings.ToLower(msg)
	for _, phrase := range contextPhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}
