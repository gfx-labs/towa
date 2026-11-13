package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gfx-labs/towa/src/provider"
)

// wireError is what a provider says about a failed request, from an error
// response's body or from an event in the stream.
type wireError struct {
	Message string
	// Type is OpenAI's error.type.
	Type string
	// Code is error.code as text. OpenAI sends a word, OpenRouter the HTTP
	// status as a number.
	Code string
	// ErrorType and LimitSource are OpenRouter's error.metadata.error_type
	// and error.metadata.limit_source.
	ErrorType   string
	LimitSource string
	// ResetsAt is OpenRouter's error.metadata.resets_at.
	ResetsAt time.Time
}

type wireErrorBody struct {
	Error   jsontext.Value `json:"error"`
	Message string         `json:"message"`
}

type wireErrorObject struct {
	Message  string         `json:"message"`
	Type     string         `json:"type"`
	Code     jsontext.Value `json:"code"`
	Metadata struct {
		ErrorType   string `json:"error_type"`
		LimitSource string `json:"limit_source"`
		ResetsAt    string `json:"resets_at"`
	} `json:"metadata"`
}

// parseErrorBody reads an error response's body. It reports false when the
// body is not JSON with an error in it.
func parseErrorBody(raw []byte) (wireError, bool) {
	var body wireErrorBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return wireError{}, false
	}
	if body.Error == nil || body.Error.Kind() == 'n' {
		if body.Message == "" {
			return wireError{}, false
		}
		return wireError{Message: body.Message}, true
	}
	return decodeError(body.Error), true
}

// decodeError reads the value of an error member: an object, or a string
// from the endpoints that send only a message.
func decodeError(v jsontext.Value) wireError {
	if v.Kind() == '"' {
		var msg string
		if json.Unmarshal(v, &msg) == nil {
			return wireError{Message: msg}
		}
	}
	var obj wireErrorObject
	if err := json.Unmarshal(v, &obj); err != nil {
		// The shape is not one this client knows. The text is kept so
		// that the error still says what the provider said.
		return wireError{Message: string(v)}
	}
	e := wireError{Message: obj.Message, Type: obj.Type, ErrorType: obj.Metadata.ErrorType, LimitSource: obj.Metadata.LimitSource}
	switch obj.Code.Kind() {
	case '"':
		_ = json.Unmarshal(obj.Code, &e.Code) // a string token always decodes
	case '0':
		e.Code = string(obj.Code)
	}
	if t, err := time.Parse(time.RFC3339, obj.Metadata.ResetsAt); err == nil {
		e.ResetsAt = t
	}
	return e
}

// classifyResponse classifies a response whose status is not 200.
func (c *Client) classifyResponse(status int, header http.Header, raw []byte) *provider.Error {
	e, ok := parseErrorBody(raw)
	if !ok {
		// A gateway's HTML or plain text. It is the only description
		// there is.
		e = wireError{Message: strings.TrimSpace(string(raw))}
	}
	return c.classifyError(status, header, e)
}

// contextFullMarks are how providers say a request does not fit the model's
// context. Most have no code for it, only a message.
var contextFullMarks = []string{
	"context_length_exceeded", // OpenAI's error.code, OpenRouter's error_type
	"maximum context length",  // OpenAI, OpenRouter, DeepSeek, vLLM
	"context window",          // Anthropic and Gemini through a gateway
	"prompt is too long",      // Anthropic through a gateway
	"exceeds the model's max", // Moonshot
}

func isContextFull(e wireError) bool {
	for _, text := range []string{e.ErrorType, e.Code, e.Type, e.Message} {
		text = strings.ToLower(text)
		for _, mark := range contextFullMarks {
			if strings.Contains(text, mark) {
				return true
			}
		}
	}
	return false
}

// classifyError decides what the caller does about a failed request. status
// is the HTTP status. For an error that arrived in a stream it is 200, and
// the status that classifies the error is the one in its code.
func (c *Client) classifyError(status int, header http.Header, e wireError) *provider.Error {
	out := &provider.Error{Status: status, Message: e.Message}
	var notes []string
	out.RetryAfter, notes = retryAfter(header, notes)
	if c.cfg.OpenRouter.Enabled {
		out.ResetAt, notes = openRouterReset(header, notes)
	}
	if out.ResetAt.IsZero() {
		out.ResetAt = e.ResetsAt
	}
	if len(notes) > 0 {
		out.Message = strings.TrimSpace(out.Message + " (" + strings.Join(notes, "; ") + ")")
	}

	effective := status
	if status == http.StatusOK {
		effective = 0
		if code, err := strconv.Atoi(e.Code); err == nil && code >= 400 && code < 600 {
			effective = code
		}
		if effective == 0 {
			effective = statusOfErrorType(e.ErrorType)
		}
	}

	set := func(class provider.Class, reason provider.Reason) *provider.Error {
		out.Class, out.Reason = class, reason
		return out
	}
	switch {
	case isContextFull(e):
		return set(provider.Fail, provider.ReasonContextFull)
	case e.ErrorType == "content_policy_violation" || e.ErrorType == "refusal":
		// The request was understood and declined. Another account gets
		// the same answer.
		return set(provider.Fail, provider.ReasonRequest)
	}
	switch effective {
	case http.StatusUnauthorized:
		return set(provider.Fallback, provider.ReasonAuth)
	case http.StatusForbidden:
		// OpenRouter answers 403 for a spent budget and names its source.
		if e.LimitSource != "" {
			return set(provider.Fallback, provider.ReasonRateLimit)
		}
		return set(provider.Fallback, provider.ReasonAuth)
	case http.StatusPaymentRequired:
		// Out of credit. Waiting helps only when the provider says how
		// long, which OpenRouter does for its in-flight budget.
		if out.RetryAfter > 0 {
			return set(provider.Retry, provider.ReasonRateLimit)
		}
		return set(provider.Fallback, provider.ReasonRateLimit)
	case http.StatusTooManyRequests:
		return set(provider.Retry, provider.ReasonRateLimit)
	case http.StatusRequestTimeout, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return set(provider.Retry, provider.ReasonOverload)
	case 0:
		// An error in the stream that names no status. The provider had
		// accepted the request, so it failed while generating.
		return set(provider.Retry, provider.ReasonOverload)
	}
	if effective >= 500 {
		return set(provider.Retry, provider.ReasonOverload)
	}
	return set(provider.Fail, provider.ReasonRequest)
}

// statusOfErrorType maps OpenRouter's error_type to the status its
// documentation gives it, for an error that carries no numeric code. It
// returns zero for a type it does not list.
func statusOfErrorType(errorType string) int {
	switch errorType {
	case "authentication":
		return http.StatusUnauthorized
	case "payment_required":
		return http.StatusPaymentRequired
	case "permission_denied":
		return http.StatusForbidden
	case "rate_limit_exceeded":
		return http.StatusTooManyRequests
	case "provider_overloaded":
		return http.StatusServiceUnavailable
	case "provider_unavailable":
		return http.StatusBadGateway
	case "timeout":
		return http.StatusGatewayTimeout
	case "server", "unmapped":
		return http.StatusInternalServerError
	case "":
		return 0
	default:
		// Every other documented type is a rejected request.
		return http.StatusBadRequest
	}
}

// retryAfter reads the Retry-After header: a number of seconds or an HTTP
// date. A value it cannot read is added to notes, so that it is seen.
func retryAfter(header http.Header, notes []string) (time.Duration, []string) {
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, notes
	}
	wait, ok := provider.ParseRetryAfter(value)
	if !ok {
		notes = append(notes, fmt.Sprintf("unreadable Retry-After %q", value))
	}
	return wait, notes
}

// openRouterReset reads X-RateLimit-Reset, which OpenRouter documents as
// milliseconds since the Unix epoch. Other providers use the same name for
// other units, so it is read only for OpenRouter.
func openRouterReset(header http.Header, notes []string) (time.Time, []string) {
	value := strings.TrimSpace(header.Get("X-RateLimit-Reset"))
	if value == "" {
		return time.Time{}, notes
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, append(notes, fmt.Sprintf("unreadable X-RateLimit-Reset %q", value))
	}
	return time.UnixMilli(ms), notes
}
