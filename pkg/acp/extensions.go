package acp

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"strings"
)

func extensionName(method string) error {
	if !strings.HasPrefix(method, "_") {
		return fmt.Errorf("acp: extension method %q must begin with _", method)
	}
	return nil
}

func (c *Connection) registerExtension(method string, handler MethodHandler, notification bool) error {
	if err := extensionName(method); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("acp: extension method %q has a nil handler", method)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	handlers := &c.extensionRequests
	if notification {
		handlers = &c.extensionNotifications
	}
	if (*handlers)[method] != nil {
		return fmt.Errorf("acp: extension method %q is already registered", method)
	}
	if *handlers == nil {
		*handlers = make(map[string]MethodHandler)
	}
	(*handlers)[method] = handler
	return nil
}

func (c *Connection) invoke(ctx context.Context, msg *inbound, notification bool) (any, *RequestError) {
	if !strings.HasPrefix(msg.Method, "_") {
		return c.handler(ctx, msg.Method, msg.Params)
	}
	c.mu.Lock()
	handler := c.extensionRequests[msg.Method]
	if notification {
		handler = c.extensionNotifications[msg.Method]
	}
	c.mu.Unlock()
	if handler == nil {
		return nil, NewMethodNotFound(msg.Method)
	}
	return handler(ctx, msg.Method, msg.Params)
}

// RegisterExtensionRequest registers a handler for an _-prefixed request.
// A duplicate registration, nil handler or non-extension name is an error.
// Registration is safe while Serve is running. Handlers run concurrently,
// like protocol request handlers. Params are valid until the handler returns.
func (a *AgentSideConnection) RegisterExtensionRequest(method string, handler MethodHandler) error {
	return a.conn.registerExtension(method, handler, false)
}

// RegisterExtensionNotification registers a handler for an _-prefixed
// notification. It runs in read order, like protocol notification handlers,
// and cannot wait for a response with its context. Its result is discarded.
// Registration has the same rules as RegisterExtensionRequest.
func (a *AgentSideConnection) RegisterExtensionNotification(method string, handler MethodHandler) error {
	return a.conn.registerExtension(method, handler, true)
}

// SendExtensionRequest sends an _-prefixed request and returns its raw JSON
// result. It follows the cancellation rules of SendRequest.
func (a *AgentSideConnection) SendExtensionRequest(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if err := extensionName(method); err != nil {
		return nil, err
	}
	return a.conn.call(ctx, method, params)
}

// SendExtensionNotification sends an _-prefixed notification in write order.
func (a *AgentSideConnection) SendExtensionNotification(ctx context.Context, method string, params any) error {
	if err := extensionName(method); err != nil {
		return err
	}
	return a.conn.SendNotification(ctx, method, params)
}

// RegisterExtensionRequest registers a handler for an _-prefixed request.
// It has the same registration and execution rules as the agent-side method.
func (c *ClientSideConnection) RegisterExtensionRequest(method string, handler MethodHandler) error {
	return c.conn.registerExtension(method, handler, false)
}

// RegisterExtensionNotification registers a handler for an _-prefixed
// notification. It has the same rules as the agent-side method.
func (c *ClientSideConnection) RegisterExtensionNotification(method string, handler MethodHandler) error {
	return c.conn.registerExtension(method, handler, true)
}

// SendExtensionRequest sends an _-prefixed request and returns its raw JSON
// result. It follows the cancellation rules of SendRequest.
func (c *ClientSideConnection) SendExtensionRequest(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if err := extensionName(method); err != nil {
		return nil, err
	}
	return c.conn.call(ctx, method, params)
}

// SendExtensionNotification sends an _-prefixed notification in write order.
func (c *ClientSideConnection) SendExtensionNotification(ctx context.Context, method string, params any) error {
	if err := extensionName(method); err != nil {
		return err
	}
	return c.conn.SendNotification(ctx, method, params)
}
