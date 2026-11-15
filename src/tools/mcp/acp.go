package mcp

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"

	acp "github.com/gfx-labs/go-acp"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ACPMessenger carries MCP messages to a server the ACP client provides,
// over the session's ACP connection (the client's mcp/message method). It is
// nil when the client named no such server.
type ACPMessenger func(ctx context.Context, params acp.UnstableMessageMcpRequest) (acp.UnstableMessageMcpResponse, error)

// acpProtocolVersion is the one MCP revision the ACP transport carries. The
// binding has no initialize handshake: every request names this version in
// its _meta, which the SDK client adds once server/discover has answered.
const acpProtocolVersion = "2026-07-28"

// acpTransport speaks MCP to a server the ACP client provides. Each MCP
// request becomes one mcp/message request on the ACP connection, and its
// response comes back as the answer. The SDK's client writes and reads
// JSON-RPC messages; this turns them into ACP calls and their answers.
//
// The binding is stateless: no MCP session, no connect or disconnect. A
// request whose context ends is cancelled on the ACP connection, which
// sends $/cancel_request for the outer call.
type acpTransport struct {
	serverID acp.UnstableMcpServerAcpId
	send     ACPMessenger
}

func (t *acpTransport) Connect(context.Context) (sdk.Connection, error) {
	if t.send == nil {
		return nil, fmt.Errorf("server %q uses the acp transport and the session has no ACP connection to send it on", t.serverID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &acpConnection{
		serverID: t.serverID, send: t.send, ctx: ctx, cancel: cancel,
		incoming: make(chan jsonrpc.Message, 16),
		calls:    map[string]context.CancelFunc{},
	}, nil
}

type acpConnection struct {
	serverID acp.UnstableMcpServerAcpId
	send     ACPMessenger
	// ctx ends when the connection closes, which cancels every call.
	ctx      context.Context
	cancel   context.CancelFunc
	incoming chan jsonrpc.Message

	mu     sync.Mutex
	closed bool
	// calls maps the inner request ID of each call in flight to the cancel
	// of its outer ACP request, for notifications/cancelled.
	calls   map[string]context.CancelFunc
	pending sync.WaitGroup
	// sequence makes the inner request IDs unique on the ACP connection,
	// which the binding requires per server.
	sequence atomic.Uint64
	prefix   string
}

var errACPConnectionClosed = errors.New("mcp: the acp connection is closed")

func (c *acpConnection) Read(ctx context.Context) (jsonrpc.Message, error) {
	select {
	case msg := <-c.incoming:
		return msg, nil
	case <-c.ctx.Done():
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *acpConnection) Write(_ context.Context, msg jsonrpc.Message) error {
	request, ok := msg.(*jsonrpc.Request)
	if !ok {
		// The server sends the agent no requests in this binding, so the
		// client has nothing to answer.
		return nil
	}
	if !request.IsCall() {
		return c.notify(request)
	}
	params, err := innerParams(request.Params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errACPConnectionClosed
	}
	if c.prefix == "" {
		c.prefix = fmt.Sprintf("towa-%p-", c)
	}
	inner := c.prefix + strconv.FormatUint(c.sequence.Add(1), 10)
	callCtx, cancel := context.WithCancel(c.ctx)
	c.calls[idKey(request.ID)] = cancel
	c.pending.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.pending.Done()
		defer func() {
			c.mu.Lock()
			delete(c.calls, idKey(request.ID))
			c.mu.Unlock()
			cancel()
		}()
		response := &jsonrpc.Response{ID: request.ID}
		answer, err := c.send(callCtx, acp.UnstableMessageMcpRequest{
			ServerId: c.serverID, RequestId: acp.UnstableMcpRequestId(inner), Method: request.Method, Params: params,
		})
		switch {
		case err != nil:
			response.Error = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcp/message: " + err.Error()}
		case answer.Result != nil:
			response.Result, err = json.Marshal(answer.Result.Result)
			if err != nil {
				response.Error = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcp/message: result: " + err.Error()}
			}
		case answer.Error != nil:
			wire := &jsonrpc.Error{Code: int64(answer.Error.Error.Code), Message: answer.Error.Error.Message}
			if answer.Error.Error.Data != nil {
				wire.Data, _ = json.Marshal(answer.Error.Error.Data)
			}
			response.Error = wire
		default:
			response.Error = &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcp/message: the answer has neither a result nor an error"}
		}
		select {
		case c.incoming <- response:
		case <-c.ctx.Done():
		}
	}()
	return nil
}

// notify handles a notification from the client. Only cancellation means
// anything here: it cancels the outer ACP request of that call.
func (c *acpConnection) notify(request *jsonrpc.Request) error {
	if request.Method != "notifications/cancelled" {
		return nil
	}
	var params struct {
		RequestID any `json:"requestId"`
	}
	if err := json.Unmarshal(request.Params, &params); err != nil {
		return nil
	}
	id, err := jsonrpc.MakeID(params.RequestID)
	if err != nil {
		return nil
	}
	c.mu.Lock()
	cancel := c.calls[idKey(id)]
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return nil
}

func (c *acpConnection) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.pending.Wait()
	return nil
}

func (c *acpConnection) SessionID() string { return "" }

func idKey(id jsonrpc.ID) string { return fmt.Sprintf("%T:%v", id.Raw(), id.Raw()) }

// innerParams decodes the params of an MCP request into the object
// mcp/message carries. Positional params are not MCP.
func innerParams(raw jsontext.Value) (map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var params map[string]any
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("mcp: request params are not a JSON object: %w", err)
	}
	return params, nil
}
