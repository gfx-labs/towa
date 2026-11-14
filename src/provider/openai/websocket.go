package openai

import (
	"context"
	"crypto/sha256"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"

	"github.com/coder/websocket"
	"github.com/valyala/bytebufferpool"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// websocketBeta selects version 2 of the Responses WebSocket protocol, the
// one jcode validated.
const websocketBeta = "responses_websockets=2026-02-06"

// socketKey names the conversation a socket is kept for. The token is part
// of it so that a socket opened with one token is never used with another.
type socketKey struct {
	base     string
	token    string
	account  string
	cacheKey string
	model    string
}

// socket is a connection kept between requests, with what the server holds
// from the last response on it.
type socket struct {
	conn *websocket.Conn
	// responseID is the last response. The next request continues from it
	// when its input starts with what that response had seen.
	responseID string
	// seen is the number of input items the server holds for responseID:
	// the input of the request and the output of the response. digest is
	// their hash. Keeping the items themselves would hold every
	// conversation in memory twice.
	seen   int
	digest [sha256.Size]byte
}

// prefixDigest hashes encoded input items, each with its length so that two
// item lists cannot collide by moving a boundary.
func prefixDigest(items []jsontext.Value) [sha256.Size]byte {
	h := sha256.New()
	var size [8]byte
	for _, item := range items {
		n := uint64(len(item))
		for i := range size {
			size[i] = byte(n >> (8 * i))
		}
		h.Write(size[:])
		h.Write(item)
	}
	var out [sha256.Size]byte
	h.Sum(out[:0])
	return out
}

func encodeItems(items []any) ([]jsontext.Value, *provider.Error) {
	out := make([]jsontext.Value, len(items))
	for i, item := range items {
		data, err := json.Marshal(item)
		if err != nil {
			return nil, invalidRequest(fmt.Sprintf("encode input item %d: %v", i, err))
		}
		out[i] = data
	}
	return out, nil
}

func values(items []jsontext.Value) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

func (c *Client) takeSocket(key socketKey) *socket {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.idle[key]
	if s != nil {
		delete(c.idle, key)
		for i, k := range c.idleOrder {
			if k == key {
				c.idleOrder = append(c.idleOrder[:i], c.idleOrder[i+1:]...)
				break
			}
		}
	}
	return s
}

// keepSocket stores s for the next request with its key. The socket idle
// the longest is closed when there are more than Config.MaxIdleSockets.
func (c *Client) keepSocket(key socketKey, s *socket) {
	var evicted []*socket
	c.mu.Lock()
	if c.closed {
		evicted = append(evicted, s)
	} else {
		if old := c.idle[key]; old != nil {
			// Two requests ran at once with one cache key. The newer
			// response is the one a next request can continue from.
			evicted = append(evicted, old)
		} else {
			c.idleOrder = append(c.idleOrder, key)
		}
		c.idle[key] = s
		for len(c.idleOrder) > c.cfg.MaxIdleSockets {
			oldest := c.idleOrder[0]
			c.idleOrder = c.idleOrder[1:]
			evicted = append(evicted, c.idle[oldest])
			delete(c.idle, oldest)
		}
	}
	c.mu.Unlock()
	for _, e := range evicted {
		_ = e.conn.Close(websocket.StatusNormalClosure, "")
	}
}

// Close closes the idle WebSocket connections. A request still running
// closes its own when it ends. The client sends no request on a kept
// connection afterwards.
func (c *Client) Close() error {
	c.mu.Lock()
	idle := c.idle
	c.idle, c.idleOrder, c.closed = map[socketKey]*socket{}, nil, true
	c.mu.Unlock()
	for _, s := range idle {
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}

func (c *Client) dial(ctx context.Context, ep endpoint, req provider.Request) (*websocket.Conn, *provider.Error) {
	header := c.header(ep, req)
	header.Set("OpenAI-Beta", websocketBeta)
	// Dial takes an https URL and upgrades it. The handshake goes through
	// our client, so it uses the proxy and trust settings of every other
	// request.
	conn, resp, err := websocket.Dial(ctx, ep.base+"/responses", &websocket.DialOptions{
		HTTPClient: c.cfg.HTTPClient,
		HTTPHeader: header,
	})
	if err != nil {
		if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
			return nil, c.statusError(ctx, resp)
		}
		return nil, provider.TransportError(ctx, "open websocket", err)
	}
	// The library's default limit of 32 KiB is below one finished response.
	// The SSE transport has no limit on an event either.
	conn.SetReadLimit(-1)
	return conn, nil
}

func (c *Client) streamWebSocket(ctx context.Context, ep endpoint, req provider.Request, p *parser, emit func(provider.Event) bool) *provider.Error {
	body, perr := c.buildRequest(ep, req)
	if perr != nil {
		return perr
	}
	input, perr := encodeItems(body.Input)
	if perr != nil {
		return perr
	}
	body.Type = "response.create"
	key := socketKey{base: ep.base, token: req.Credential.Token, account: req.Credential.Account, cacheKey: req.CacheKey, model: req.Model}

	var sock *socket
	if req.CacheKey != "" {
		sock = c.takeSocket(key)
	}
	for {
		reused := sock != nil
		if !reused {
			conn, perr := c.dial(ctx, ep, req)
			if perr != nil {
				return perr
			}
			sock = &socket{conn: conn}
		}
		// The server already holds the first seen items. Only what follows
		// is sent, and a history that was rewritten is sent whole.
		body.Input, body.PreviousResponseID = values(input), ""
		if reused && sock.seen < len(input) && prefixDigest(input[:sock.seen]) == sock.digest {
			body.Input, body.PreviousResponseID = values(input[sock.seen:]), sock.responseID
		}
		emitted, perr := c.exchange(ctx, sock, body, p, emit)
		if perr == nil && !p.stopped {
			c.finishSocket(key, sock, ep.provider, input, p)
			return nil
		}
		_ = sock.conn.CloseNow()
		// The server closes a connection that sat idle, and the client
		// learns of it only now. Nothing was yielded, so the request is
		// sent once more on a new connection.
		if perr != nil && reused && !emitted && perr.Reason == provider.ReasonNetwork && ctx.Err() == nil {
			sock = nil
			continue
		}
		return perr
	}
}

// exchange sends one request on the socket and reads its response. It
// reports whether any event of the response was read.
func (c *Client) exchange(ctx context.Context, sock *socket, body *requestBody, p *parser, emit func(provider.Event) bool) (bool, *provider.Error) {
	buf := bytebufferpool.Get()
	err := json.MarshalWrite(buf, body)
	if err == nil {
		err = sock.conn.Write(ctx, websocket.MessageText, buf.B)
	}
	bytebufferpool.Put(buf)
	if err != nil {
		return false, provider.TransportError(ctx, "send request on websocket", err)
	}
	read := false
	for {
		_, data, err := sock.conn.Read(ctx)
		if err != nil {
			return read, provider.TransportError(ctx, "read websocket", err)
		}
		read = true
		done, perr := p.handle(data, emit)
		if perr != nil || done || p.stopped {
			return read, perr
		}
	}
}

// finishSocket records what the server now holds and keeps the socket.
func (c *Client) finishSocket(key socketKey, sock *socket, own string, input []jsontext.Value, p *parser) {
	keep := key.cacheKey != "" && p.responseID != ""
	var held []jsontext.Value
	if keep {
		// The next request renders this response's items from the
		// transcript. They are rendered the same way here, so that the
		// prefix it sends hashes to the same value.
		rendered, perr := buildInput(own, transcript.Origin{Model: p.model, Account: p.account}, p.out)
		if perr == nil {
			var out []jsontext.Value
			if out, perr = encodeItems(rendered); perr == nil {
				held = slices.Concat(input, out)
			}
		}
		keep = perr == nil
	}
	if !keep {
		_ = sock.conn.Close(websocket.StatusNormalClosure, "")
		return
	}
	sock.responseID, sock.seen, sock.digest = p.responseID, len(held), prefixDigest(held)
	c.keepSocket(key, sock)
}
