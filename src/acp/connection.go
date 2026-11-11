package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
)

// methodCancelRequest is the protocol-level notification that cancels one
// request by id. See https://agentclientprotocol.com/protocol/v1/cancellation.
const methodCancelRequest = "$/cancel_request"

// ErrClosed is returned by calls on a connection whose reader has ended.
var ErrClosed = errors.New("acp: connection closed")

// ErrWaitInNotification is returned when a request is sent with the context
// of a notification handler. Notifications are handled on the reading
// goroutine, which is the one that would read the response, so the call
// could never return. A handler that must call the peer starts a goroutine
// with a context of its own.
var ErrWaitInNotification = errors.New("acp: a notification handler cannot wait for a response from the peer")

// errEmptyResponse is returned to the caller of a request whose response
// carried neither a result nor an error.
var errEmptyResponse = errors.New("acp: response has neither result nor error")

// MethodHandler handles one inbound request or notification. For a
// notification the result is discarded.
type MethodHandler func(ctx context.Context, method string, params json.RawMessage) (any, *RequestError)

// message is one JSON-RPC 2.0 message of any kind.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RequestError   `json:"error,omitempty"`
}

type cancelRequestParams struct {
	RequestID json.RawMessage `json:"requestId"`
}

type response struct {
	result json.RawMessage
	err    error
}

type (
	arrivalKey      struct{}
	notificationKey struct{}
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
// request is handled on its own goroutine.
type Connection struct {
	handler MethodHandler
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

// Serve reads and handles messages until the reader ends or fails. It
// returns nil when the peer closed the stream between messages. ctx is the
// parent of every handler's context. Cancelling ctx does not interrupt a
// blocked read: close the reader to stop Serve. Before it returns, Serve
// cancels the handlers still running and waits for them, which includes
// writing their responses: close the writer if the peer has stopped reading.
//
// A message is read whole, whatever its length. Prompts carry files and
// images inline, and the protocol sets no limit.
func (c *Connection) Serve(ctx context.Context) error {
	if c.served.Swap(true) {
		return errors.New("acp: Serve called twice")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	br := bufio.NewReader(c.r)
	var arrival uint64
	var readErr error
	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			switch {
			case !errors.Is(err, io.EOF):
				readErr = err
			case len(bytes.TrimSpace(line)) > 0:
				readErr = io.ErrUnexpectedEOF
			}
			break
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		arrival++
		c.dispatch(context.WithValue(ctx, arrivalKey{}, arrival), line)
	}

	c.mu.Lock()
	c.closeErr = ErrClosed
	if readErr != nil {
		c.closeErr = fmt.Errorf("%w: %w", ErrClosed, readErr)
	}
	c.mu.Unlock()
	close(c.done)

	cancel()
	c.handlers.Wait()
	return readErr
}

func (c *Connection) dispatch(ctx context.Context, line []byte) {
	var msg message
	if err := json.Unmarshal(line, &msg); err != nil {
		rerr := NewInvalidRequest(map[string]any{"error": err.Error()})
		if !json.Valid(line) {
			rerr = NewParseError(map[string]any{"error": err.Error()})
		}
		c.logger.Warn("acp: unreadable message", "err", err)
		c.respond(json.RawMessage("null"), nil, rerr)
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

func (c *Connection) handleNotification(ctx context.Context, msg *message) {
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
	_, rerr := c.handler(context.WithValue(ctx, notificationKey{}, true), msg.Method, msg.Params)
	switch {
	case rerr == nil:
	case rerr.Code == codeMethodNotFound:
		// The protocol says unrecognized notifications are ignored.
		c.logger.Debug("acp: unhandled notification", "method", msg.Method)
	default:
		c.logger.Warn("acp: notification handler failed", "method", msg.Method, "err", rerr)
	}
}

func (c *Connection) handleRequest(ctx context.Context, msg *message) {
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
		result, rerr := c.handler(ctx, msg.Method, msg.Params)
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
		cancel()
		c.respond(msg.ID, result, rerr)
	})
}

func (c *Connection) handleResponse(msg *message) {
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
func (c *Connection) respond(id json.RawMessage, result any, rerr *RequestError) {
	msg := message{JSONRPC: "2.0", ID: id, Error: rerr}
	if rerr == nil {
		raw, err := marshal(result)
		if err != nil {
			msg.Error = NewInternalError(map[string]any{"error": err.Error()})
		} else {
			msg.Result = raw
		}
	}
	if err := c.write(&msg); err != nil {
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
	raw, err := marshalParams(params)
	if err != nil {
		return fmt.Errorf("acp: encode %s params: %w", method, err)
	}
	return c.write(&message{JSONRPC: "2.0", Method: method, Params: raw})
}

func (c *Connection) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx.Value(notificationKey{}) != nil {
		return nil, ErrWaitInNotification
	}
	raw, err := marshalParams(params)
	if err != nil {
		return nil, fmt.Errorf("acp: encode %s params: %w", method, err)
	}
	id := c.nextID.Add(1)
	idRaw := json.RawMessage(strconv.FormatUint(id, 10))
	ch := make(chan response, 1)

	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.closed(); err != nil {
		c.forget(id)
		return nil, err
	}

	if err := c.write(&message{JSONRPC: "2.0", ID: idRaw, Method: method, Params: raw}); err != nil {
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

func (r response) unwrap() (json.RawMessage, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.result, nil
}

// write sends one message as a single Write, so a message is never split by
// another goroutine's message.
func (c *Connection) write(msg *message) error {
	b, err := marshal(msg)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err = c.w.Write(b)
	return err
}

// marshal encodes v on one line. It leaves <, > and & as they are: the
// messages carry source code, and the default escaping makes each of those
// six bytes long.
func marshal(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// marshalParams encodes params, leaving the field out when there are none.
func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return marshal(params)
}
