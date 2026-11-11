package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/valyala/bytebufferpool"
)

// methodCancelRequest is the protocol-level notification that cancels one
// request by id. See https://agentclientprotocol.com/protocol/v1/cancellation.
const methodCancelRequest = "$/cancel_request"

// ErrClosed is returned by calls on a connection whose reader has ended.
var ErrClosed = errors.New("acp: connection closed")

// ErrWaitInOrder is returned when a request is sent with the context of a
// handler that runs in order: a notification handler, or the agent's
// initialize handler. Those run on the goroutine that runs Serve, which is
// the one that would deliver the response, so the call could never return. A
// handler that must call the peer starts a goroutine with a context of its
// own.
var ErrWaitInOrder = errors.New("acp: a handler that runs in order cannot wait for a response from the peer")

// errEmptyResponse is returned to the caller of a request whose response
// carried neither a result nor an error.
var errEmptyResponse = errors.New("acp: response has neither result nor error")

// MethodHandler handles one inbound request or notification. For a
// notification the result is discarded.
//
// params is only valid until the handler returns.
type MethodHandler func(ctx context.Context, method string, params jsontext.Value) (any, *RequestError)

// version is the value of every message's jsonrpc member.
const version = "2.0"

// inbound is a JSON-RPC 2.0 message of any kind, as read. A member that is
// absent leaves its field nil. A member that is null leaves it "null".
type inbound struct {
	ID     jsontext.Value `json:"id"`
	Method string         `json:"method"`
	Params jsontext.Value `json:"params"`
	Result jsontext.Value `json:"result"`
	Error  *RequestError  `json:"error"`
}

// The outbound messages hold the params and the result as Go values, so a
// message is encoded once, into one buffer. The members are written in the
// order jsonrpc, id, method, params, result, error.

// outboundCall is a request, or a notification when ID is nil.
type outboundCall struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id,omitzero"`
	Method  string         `json:"method"`
	Params  any            `json:"params,omitzero"`
}

// outboundResult is a response that carries a result. A nil result is
// written as null: a response needs a result or an error.
type outboundResult struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Result  any            `json:"result"`
}

// outboundError is a response that carries an error.
type outboundError struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      jsontext.Value `json:"id"`
	Error   *RequestError  `json:"error"`
}

type cancelRequestParams struct {
	RequestID jsontext.Value `json:"requestId"`
}

type response struct {
	result jsontext.Value
	err    error
}

// wireOptions are the options every outbound message is encoded with. They
// keep the bytes what encoding/json v1 wrote with HTML escaping off: the
// members of a map in sorted order, and U+2028 and U+2029 escaped. <, > and &
// are left as they are: the messages carry source code, and escaping makes
// each of those six bytes long.
var wireOptions = json.JoinOptions(json.Deterministic(true), jsontext.EscapeForJS(true))

type (
	arrivalKey struct{}
	inOrderKey struct{}
)

// Arrival returns the position of the inbound message that ctx's handler is
// handling, counted from 1 in the order the messages were read. Handlers of
// requests run concurrently, so a handler uses Arrival to order its request
// against other messages, for example a session/cancel against a prompt.
func Arrival(ctx context.Context) (uint64, bool) {
	n, ok := ctx.Value(arrivalKey{}).(uint64)
	return n, ok
}

// Connection is a JSON-RPC 2.0 connection over newline-delimited messages.
//
// Inbound notifications and responses are handled one at a time on the
// goroutine that runs Serve, in the order they were read. Each inbound
// request is handled on its own goroutine. A second goroutine reads the
// messages, so that Serve can stop while a read is blocked.
type Connection struct {
	handler MethodHandler
	// inOrder names the requests that are handled like notifications: on
	// the goroutine that runs Serve, finished before the next message is
	// looked at. Nil means none.
	inOrder func(method string) bool
	logger  *slog.Logger
	r       io.Reader

	writeMu sync.Mutex
	w       io.Writer

	nextID atomic.Uint64
	served atomic.Bool

	mu       sync.Mutex
	pending  map[uint64]chan response
	inflight map[string]context.CancelFunc
	closeErr error

	done     chan struct{}
	handlers sync.WaitGroup
}

// NewConnection returns a connection that writes to w and reads from r once
// Serve is called. A nil logger discards log records.
func NewConnection(handler MethodHandler, w io.Writer, r io.Reader, logger *slog.Logger) *Connection {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Connection{
		handler:  handler,
		logger:   logger,
		r:        r,
		w:        w,
		pending:  map[uint64]chan response{},
		inflight: map[string]context.CancelFunc{},
		done:     make(chan struct{}),
	}
}

// Done is closed when Serve has stopped reading.
func (c *Connection) Done() <-chan struct{} { return c.done }

// Serve handles messages until the reader ends or fails, or ctx is
// cancelled. It returns nil when the peer closed the stream between
// messages, and ctx's error when ctx ended first. ctx is the parent of every
// handler's context. Before it returns, Serve cancels the handlers still
// running and waits for them, which includes writing their responses: close
// the writer if the peer has stopped reading.
//
// A read that is blocked when ctx is cancelled stays blocked, with its
// goroutine, until the reader returns. Standard input cannot be interrupted,
// so a process that stops this way exits. Close any other reader.
//
// A message is read whole, whatever its length. Prompts carry files and
// images inline, and the protocol sets no limit.
func (c *Connection) Serve(ctx context.Context) error {
	if c.served.Swap(true) {
		return errors.New("acp: Serve called twice")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	lines := make(chan *bytebufferpool.ByteBuffer)
	readDone := make(chan error, 1)
	go func() { readDone <- c.read(lines) }()

	var arrival uint64
	var err error
	for serving := true; serving; {
		select {
		case line := <-lines:
			arrival++
			c.dispatchLine(context.WithValue(ctx, arrivalKey{}, arrival), line)
		case err = <-readDone:
			serving = false
		case <-ctx.Done():
			err = context.Cause(ctx)
			serving = false
		}
	}

	c.mu.Lock()
	c.closeErr = ErrClosed
	if err != nil {
		c.closeErr = fmt.Errorf("%w: %w", ErrClosed, err)
	}
	c.mu.Unlock()
	close(c.done)

	cancel()
	c.handlers.Wait()
	return err
}

// read passes each message to lines until the reader ends or the connection
// is done. It returns nil when the reader ended between messages.
func (c *Connection) read(lines chan<- *bytebufferpool.ByteBuffer) error {
	br := bufio.NewReader(c.r)
	for {
		line, err := readLine(br)
		if err != nil {
			partial := len(bytes.TrimSpace(line.B)) > 0
			bytebufferpool.Put(line)
			switch {
			case !errors.Is(err, io.EOF):
				return err
			case partial:
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if len(bytes.TrimSpace(line.B)) == 0 {
			bytebufferpool.Put(line)
			continue
		}
		select {
		case lines <- line:
		case <-c.done:
			bytebufferpool.Put(line)
			return nil
		}
	}
}

// Ownership passes to Serve after read sends the buffer. Unmarshal copies
// raw values, so neither a request goroutine nor a pending response keeps it.
func (c *Connection) dispatchLine(ctx context.Context, line *bytebufferpool.ByteBuffer) {
	defer bytebufferpool.Put(line)
	c.dispatch(ctx, bytes.TrimSpace(line.B))
}

func readLine(br *bufio.Reader) (*bytebufferpool.ByteBuffer, error) {
	line := bytebufferpool.Get()
	for {
		part, err := br.ReadSlice('\n')
		line.B = append(line.B, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func (c *Connection) dispatch(ctx context.Context, line []byte) {
	var msg inbound
	if err := json.Unmarshal(line, &msg); err != nil {
		rerr := NewInvalidRequest(map[string]any{"error": err.Error()})
		// A syntactic error is JSON that cannot be read: malformed, invalid
		// UTF-8, or an object with the same member name twice.
		var syntax *jsontext.SyntacticError
		if errors.As(err, &syntax) {
			rerr = NewParseError(map[string]any{"error": err.Error()})
		}
		c.logger.Warn("acp: unreadable message", "err", err)
		c.respond(jsontext.Value("null"), nil, rerr)
		return
	}
	switch {
	case msg.Method != "" && msg.ID == nil:
		c.handleNotification(ctx, &msg)
	case msg.Method != "":
		c.handleRequest(ctx, &msg)
	case msg.ID != nil:
		c.handleResponse(&msg)
	default:
		c.logger.Warn("acp: message has neither method nor id")
	}
}

func (c *Connection) handleNotification(ctx context.Context, msg *inbound) {
	if msg.Method == methodCancelRequest {
		var p cancelRequestParams
		if err := json.Unmarshal(msg.Params, &p); err != nil || p.RequestID == nil {
			c.logger.Warn("acp: malformed "+methodCancelRequest, "err", err)
			return
		}
		c.mu.Lock()
		cancel := c.inflight[string(p.RequestID)]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return
	}
	_, rerr := c.handler(context.WithValue(ctx, inOrderKey{}, true), msg.Method, msg.Params)
	switch {
	case rerr == nil:
	case rerr.Code == codeMethodNotFound:
		// The protocol says unrecognized notifications are ignored.
		c.logger.Debug("acp: unhandled notification", "method", msg.Method)
	default:
		c.logger.Warn("acp: notification handler failed", "method", msg.Method, "err", rerr)
	}
}

func (c *Connection) handleRequest(ctx context.Context, msg *inbound) {
	after := &afterResponse{}
	ctx = context.WithValue(ctx, afterResponseKey{}, after)
	if c.inOrder != nil && c.inOrder(msg.Method) {
		result, rerr := c.handler(context.WithValue(ctx, inOrderKey{}, true), msg.Method, msg.Params)
		c.respond(msg.ID, result, rerr)
		after.run()
		return
	}
	key := string(msg.ID)
	ctx, cancel := context.WithCancel(ctx)

	c.mu.Lock()
	_, duplicate := c.inflight[key]
	if !duplicate {
		c.inflight[key] = cancel
	}
	c.mu.Unlock()
	if duplicate {
		cancel()
		c.respond(msg.ID, nil, NewInvalidRequest(map[string]any{"error": "request id is already in use"}))
		return
	}

	c.handlers.Go(func() {
		defer cancel()
		result, rerr := c.handler(ctx, msg.Method, msg.Params)
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		c.respond(msg.ID, result, rerr)
		after.run()
	})
}

func (c *Connection) handleResponse(msg *inbound) {
	id, err := strconv.ParseUint(string(msg.ID), 10, 64)
	if err != nil {
		c.logger.Warn("acp: response with an id this side never sent", "id", string(msg.ID))
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		// The caller stopped waiting, or the peer answered twice.
		c.logger.Debug("acp: response without a waiting request", "id", id)
		return
	}
	switch {
	case msg.Error != nil:
		ch <- response{err: msg.Error}
	case msg.Result != nil:
		ch <- response{result: msg.Result}
	default:
		ch <- response{err: errEmptyResponse}
	}
}

// respond answers the inbound request id with result, or with rerr when set.
func (c *Connection) respond(id jsontext.Value, result any, rerr *RequestError) {
	var err error
	if rerr == nil {
		var encodeErr error
		if err, encodeErr = c.write(&outboundResult{JSONRPC: version, ID: id, Result: result}); encodeErr != nil {
			// Nothing was written, so the request can still be answered.
			rerr = NewInternalError(map[string]any{"error": encodeErr.Error()})
		}
	}
	if rerr != nil {
		err, _ = c.write(&outboundError{JSONRPC: version, ID: id, Error: rerr})
	}
	if err != nil {
		c.logger.Debug("acp: response not written", "err", err)
	}
}

// SendRequest sends a request and waits for its response, decoded into T.
// When ctx ends first, it asks the peer to cancel the request and returns
// ctx's error without waiting for the peer's answer.
func SendRequest[T any](ctx context.Context, c *Connection, method string, params any) (T, error) {
	var out T
	raw, err := c.call(ctx, method, params)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("acp: decode %s response: %w", method, err)
	}
	return out, nil
}

// SendRequestNoResult sends a request whose response carries no data.
func (c *Connection) SendRequestNoResult(ctx context.Context, method string, params any) error {
	_, err := c.call(ctx, method, params)
	return err
}

// SendNotification sends a notification. It returns once the message has
// been written, so notifications reach the peer in the order they were sent.
func (c *Connection) SendNotification(_ context.Context, method string, params any) error {
	return c.send(&outboundCall{JSONRPC: version, Method: method, Params: params})
}

// send writes a request or a notification.
func (c *Connection) send(msg *outboundCall) error {
	err, encodeErr := c.write(msg)
	if encodeErr != nil {
		return fmt.Errorf("acp: encode %s: %w", msg.Method, encodeErr)
	}
	return err
}

func (c *Connection) call(ctx context.Context, method string, params any) (jsontext.Value, error) {
	if ctx.Value(inOrderKey{}) != nil {
		return nil, ErrWaitInOrder
	}
	id := c.nextID.Add(1)
	idRaw := jsontext.Value(strconv.FormatUint(id, 10))
	ch := make(chan response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.closed(); err != nil {
		c.forget(id)
		return nil, err
	}

	if err := c.send(&outboundCall{JSONRPC: version, ID: idRaw, Method: method, Params: params}); err != nil {
		c.forget(id)
		return nil, err
	}

	select {
	case resp := <-ch:
		return resp.unwrap()
	case <-ctx.Done():
		c.forget(id)
		// Serve cancels its handlers once it has stopped reading, so a
		// handler's call can see its context end before it sees done.
		if err := c.closed(); err != nil {
			return nil, err
		}
		if err := c.SendNotification(ctx, methodCancelRequest, cancelRequestParams{RequestID: idRaw}); err != nil {
			c.logger.Debug("acp: cancel not written", "method", method, "err", err)
		}
		return nil, context.Cause(ctx)
	case <-c.done:
		// A response read just before the reader ended still counts.
		select {
		case resp := <-ch:
			return resp.unwrap()
		default:
		}
		c.forget(id)
		return nil, c.closed()
	}
}

// closed returns the error for a connection that has stopped reading, or nil
// while it is still reading.
func (c *Connection) closed() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closeErr
}

func (c *Connection) forget(id uint64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (r response) unwrap() (jsontext.Value, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

// write encodes msg and sends it as a single Write, so a message is never
// split by another goroutine's message. The message is encoded whole before
// any of it is written: when it cannot be encoded, encodeErr says why and
// nothing has reached the peer.
func (c *Connection) write(msg any) (err, encodeErr error) {
	buf := bytebufferpool.Get()
	defer bytebufferpool.Put(buf)
	if err := json.MarshalWrite(buf, msg, wireOptions); err != nil {
		return nil, err
	}
	buf.B = append(buf.B, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(buf.B)
	return err, nil
}
