package mcp

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"

	acp "github.com/gfx-labs/go-acp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Elicitation passes a server's elicitations on to the ACP client, which
// asks the user. Form and URL are the modes the client advertised, and
// servers are told only those.
type Elicitation struct {
	Form bool
	URL  bool
	// Create sends elicitation/create. The caller scopes it to its session.
	Create func(ctx context.Context, params acp.CreateElicitationRequest) (acp.CreateElicitationResponse, error)
	// Complete sends elicitation/complete for a URL elicitation that the
	// server says has finished.
	Complete func(ctx context.Context, params acp.CompleteElicitationNotification) error
}

// options sets the client options that take a server's elicitations.
// The SDK checks a request's mode, schema and URL before calling the
// handler, and checks accepted content against the schema after.
func (e *Elicitation) options(o *sdk.ClientOptions, logger *slog.Logger) {
	caps := &sdk.ElicitationCapabilities{}
	if e.Form {
		caps.Form = &sdk.FormElicitationCapabilities{}
	}
	if e.URL {
		caps.URL = &sdk.URLElicitationCapabilities{}
	}
	// This replaces the SDK's default, roots, which is deprecated and which
	// the harness has none of.
	o.Capabilities = &sdk.ClientCapabilities{Elicitation: caps}
	o.ElicitationHandler = e.create
	o.ElicitationCompleteHandler = func(ctx context.Context, req *sdk.ElicitationCompleteNotificationRequest) {
		err := e.Complete(ctx, acp.CompleteElicitationNotification{ElicitationId: acp.ElicitationId(req.Params.ElicitationID)})
		if err != nil {
			logger.ErrorContext(ctx, "forwarding elicitation completion", "elicitation", req.Params.ElicitationID, "err", err)
		}
	}
}

func (e *Elicitation) create(ctx context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
	var params acp.CreateElicitationRequest
	switch p := req.Params; p.Mode {
	case "", "form":
		if !e.Form {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "the client does not take form elicitation"}
		}
		schema := acp.ElicitationSchema{Type: acp.ElicitationSchemaTypeObject, Properties: map[string]any{}}
		if p.RequestedSchema != nil {
			encoded, err := json.Marshal(p.RequestedSchema)
			if err != nil {
				return nil, err
			}
			if err := json.Unmarshal(encoded, &schema); err != nil {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("requestedSchema: %v", err)}
			}
		}
		params.Form = &acp.CreateElicitationForm{Message: p.Message, RequestedSchema: schema}
	case "url":
		if !e.URL {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "the client does not take URL elicitation"}
		}
		params.Url = &acp.CreateElicitationUrl{Message: p.Message, Url: p.URL, ElicitationId: acp.ElicitationId(p.ElicitationID)}
	default:
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unsupported elicitation mode %q", p.Mode)}
	}
	resp, err := e.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("mcp: elicitation/create to the client: %w", err)
	}
	switch {
	case resp.Accept != nil:
		return &sdk.ElicitResult{Action: "accept", Content: resp.Accept.Content}, nil
	case resp.Decline != nil:
		return &sdk.ElicitResult{Action: "decline"}, nil
	case resp.Cancel != nil:
		return &sdk.ElicitResult{Action: "cancel"}, nil
	}
	return nil, errors.New("mcp: the client answered elicitation/create with an unknown action")
}
