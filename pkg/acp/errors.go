package acp

import (
	"context"
	"errors"
	"fmt"
)

// JSON-RPC and ACP error codes, as listed by ErrorCode in the schema.
const (
	codeParseError       = -32700
	codeInvalidRequest   = -32600
	codeMethodNotFound   = -32601
	codeInvalidParams    = -32602
	codeInternalError    = -32603
	codeRequestCancelled = -32800
	codeAuthRequired     = -32000
	codeResourceNotFound = -32002
)

// RequestError is a JSON-RPC error object. A handler that returns one sends
// it to the peer unchanged.
type RequestError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitzero"`
}

// Error implements error.
func (e *RequestError) Error() string {
	if e.Data != nil {
		return fmt.Sprintf("code %d: %s (data: %v)", e.Code, e.Message, e.Data)
	}
	return fmt.Sprintf("code %d: %s", e.Code, e.Message)
}

// NewParseError reports a message that is not valid JSON.
func NewParseError(data any) *RequestError {
	return &RequestError{Code: codeParseError, Message: "Parse error", Data: data}
}

// NewInvalidRequest reports a message that is not a valid JSON-RPC message.
func NewInvalidRequest(data any) *RequestError {
	return &RequestError{Code: codeInvalidRequest, Message: "Invalid request", Data: data}
}

// NewMethodNotFound reports a method the receiver does not implement.
func NewMethodNotFound(method string) *RequestError {
	return &RequestError{Code: codeMethodNotFound, Message: "Method not found", Data: map[string]any{"method": method}}
}

// NewInvalidParams reports parameters that do not fit the method.
func NewInvalidParams(data any) *RequestError {
	return &RequestError{Code: codeInvalidParams, Message: "Invalid params", Data: data}
}

// NewInternalError reports a failure inside the receiver.
func NewInternalError(data any) *RequestError {
	return &RequestError{Code: codeInternalError, Message: "Internal error", Data: data}
}

// NewRequestCancelled reports a request that was cancelled before it finished.
func NewRequestCancelled(data any) *RequestError {
	return &RequestError{Code: codeRequestCancelled, Message: "Request cancelled", Data: data}
}

// NewAuthRequired reports that the method needs authentication first.
func NewAuthRequired(data any) *RequestError {
	return &RequestError{Code: codeAuthRequired, Message: "Authentication required", Data: data}
}

// NewResourceNotFound reports a resource, such as a session, that does not exist.
func NewResourceNotFound(data any) *RequestError {
	return &RequestError{Code: codeResourceNotFound, Message: "Resource not found", Data: data}
}

// toReqErr turns a handler's error into the error object sent to the peer.
func toReqErr(err error) *RequestError {
	if err == nil {
		return nil
	}
	var re *RequestError
	if errors.As(err, &re) {
		return re
	}
	if errors.Is(err, context.Canceled) {
		return NewRequestCancelled(map[string]any{"error": err.Error()})
	}
	return NewInternalError(map[string]any{"error": err.Error()})
}
