package acp_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/acp"
)

// The tests run in synctest bubbles so that a message that never arrives
// fails the test as a deadlock at once, instead of hanging until the timeout.

func TestPromptTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag, cl := &agent{}, &client{}
		agentConn, clientConn := connect(t, ag, cl)

		var mu sync.Mutex
		var seen []string
		record := func(s string) {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, s)
		}

		cl.sessionUpdate = func(_ context.Context, n acp.SessionNotification) error {
			record("update " + n.Update.AgentMessageChunk.Content.Text.Text)
			return nil
		}
		cl.readTextFile = func(_ context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
			record("read " + p.Path)
			return acp.ReadTextFileResponse{Content: "body"}, nil
		}
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			for _, text := range []string{"one", "two"} {
				if err := agentConn.SessionUpdate(ctx, agentMessage(p.SessionId, text)); err != nil {
					return acp.PromptResponse{}, err
				}
			}
			// The client's answer arrives while this request is still being
			// handled, so requests must not be handled on the reading goroutine.
			file, err := agentConn.ReadTextFile(ctx, acp.ReadTextFileRequest{SessionId: p.SessionId, Path: "/a.txt"})
			if err != nil {
				return acp.PromptResponse{}, err
			}
			if err := agentConn.SessionUpdate(ctx, agentMessage(p.SessionId, file.Content)); err != nil {
				return acp.PromptResponse{}, err
			}
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		resp, err := clientConn.Prompt(t.Context(), acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{textBlock("hi")}})
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

		// Every update was delivered, in order, before Prompt returned.
		mu.Lock()
		defer mu.Unlock()
		require.Equal(t, []string{"update one", "update two", "read /a.txt", "update body"}, seen)
	})
}

func TestEmptyRequiredStringIsAccepted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag, cl := &agent{}, &client{}
		agentConn, clientConn := connect(t, ag, cl)

		// Truncating a file sends content "". The schema requires the field
		// to be present, not to be non-empty.
		written := make(chan acp.WriteTextFileRequest, 1)
		cl.writeTextFile = func(_ context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
			written <- p
			return acp.WriteTextFileResponse{}, nil
		}
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			_, err := agentConn.WriteTextFile(ctx, acp.WriteTextFileRequest{SessionId: p.SessionId, Path: "/a.txt", Content: ""})
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, err
		}

		_, err := clientConn.Prompt(t.Context(), acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{}})
		require.NoError(t, err)
		require.Equal(t, acp.WriteTextFileRequest{SessionId: "s", Path: "/a.txt", Content: ""}, <-written)
	})
}

func TestMessageLargerThanScannerLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag := &agent{}
		_, clientConn := connect(t, ag, &client{})

		// bufio.Scanner refuses lines over this size by default. Prompts with
		// inline files are routinely larger.
		text := strings.Repeat("x", 2*bufio.MaxScanTokenSize)
		got := make(chan string, 1)
		ag.prompt = func(_ context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			got <- p.Prompt[0].Text.Text
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
		}

		_, err := clientConn.Prompt(t.Context(), acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{textBlock(text)}})
		require.NoError(t, err)
		require.Equal(t, text, <-got)
	})
}

func TestErrorObjectReachesCaller(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag, cl := &agent{}, &client{}
		agentConn, clientConn := connect(t, ag, cl)

		cl.readTextFile = func(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
			return acp.ReadTextFileResponse{}, acp.NewResourceNotFound(map[string]any{"path": "/missing"})
		}
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			_, err := agentConn.ReadTextFile(ctx, acp.ReadTextFileRequest{SessionId: p.SessionId, Path: "/missing"})
			return acp.PromptResponse{}, err
		}

		// The client's error travels to the agent, which returns it, and it
		// travels back to the client unchanged.
		_, err := clientConn.Prompt(t.Context(), acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{}})
		var rerr *acp.RequestError
		require.ErrorAs(t, err, &rerr)
		require.Equal(t, acp.NewResourceNotFound(nil).Code, rerr.Code)
		require.Equal(t, map[string]any{"path": "/missing"}, rerr.Data)
	})
}

func TestCallerCancellationReachesHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag := &agent{}
		_, clientConn := connect(t, ag, &client{})

		started, stopped := make(chan struct{}), make(chan struct{})
		ag.prompt = func(ctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
			close(started)
			<-ctx.Done()
			close(stopped)
			return acp.PromptResponse{}, ctx.Err()
		}
		// The generated client also sends session/cancel when a prompt's
		// context ends.
		ag.cancel = func(context.Context, acp.CancelNotification) error { return nil }

		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			<-started
			cancel()
		}()
		_, err := clientConn.Prompt(ctx, acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{}})
		require.ErrorIs(t, err, context.Canceled)
		<-stopped
	})
}

func TestCancelRequestNotification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag := &agent{}
		_, w := serveAgent(t, ag)

		started := make(chan struct{})
		ag.prompt = func(ctx context.Context, _ acp.PromptRequest) (acp.PromptResponse, error) {
			close(started)
			<-ctx.Done()
			return acp.PromptResponse{}, ctx.Err()
		}

		w.send(`{"jsonrpc":"2.0","id":"p-1","method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)
		<-started
		w.send(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"p-1"}}`)

		got := w.recv()
		require.JSONEq(t, `"p-1"`, string(got.ID))
		require.NotNil(t, got.Error)
		require.Equal(t, acp.NewRequestCancelled(nil).Code, got.Error.Code)
	})
}

func TestArrivalOrdersCancelAgainstPrompts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag := &agent{}
		_, w := serveAgent(t, ag)

		type seen struct {
			what    string
			arrival uint64
		}
		arrivals := make(chan seen, 3)
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			n, _ := acp.Arrival(ctx)
			arrivals <- seen{p.Prompt[0].Text.Text, n}
			<-ctx.Done()
			return acp.PromptResponse{}, ctx.Err()
		}
		ag.cancel = func(ctx context.Context, _ acp.CancelNotification) error {
			n, _ := acp.Arrival(ctx)
			arrivals <- seen{"cancel", n}
			return nil
		}

		// The two prompts run concurrently with each other and with the
		// cancel, so only Arrival tells the agent which prompt the cancel is for.
		w.send(
			`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"first"}]}}`,
			`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}`,
			`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"s","prompt":[{"type":"text","text":"second"}]}}`,
		)

		got := map[string]uint64{}
		for range 3 {
			s := <-arrivals
			got[s.what] = s.arrival
		}
		require.Equal(t, map[string]uint64{"first": 1, "cancel": 2, "second": 3}, got)
	})
}

func TestErrorResponses(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantID   string
		wantCode int
	}{
		{
			name:     "not JSON",
			line:     `{"jsonrpc":"2.0","id":1,`,
			wantID:   `null`,
			wantCode: acp.NewParseError(nil).Code,
		},
		{
			name:     "JSON that is not a message",
			line:     `[]`,
			wantID:   `null`,
			wantCode: acp.NewInvalidRequest(nil).Code,
		},
		{
			name:     "unknown method",
			line:     `{"jsonrpc":"2.0","id":7,"method":"session/unknown","params":{}}`,
			wantID:   `7`,
			wantCode: acp.NewMethodNotFound("").Code,
		},
		{
			name:     "params missing a required field",
			line:     `{"jsonrpc":"2.0","id":"a","method":"session/prompt","params":{"sessionId":"s"}}`,
			wantID:   `"a"`,
			wantCode: acp.NewInvalidParams(nil).Code,
		},
		{
			name:     "handler returns a plain error",
			line:     `{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"protocolVersion":1}}`,
			wantID:   `8`,
			wantCode: acp.NewInternalError(nil).Code,
		},
		{
			name:     "handler returns an error object",
			line:     `{"jsonrpc":"2.0","id":9,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`,
			wantID:   `9`,
			wantCode: acp.NewAuthRequired(nil).Code,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, w := serveAgent(t, &agent{
					initialize: func(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
						return acp.InitializeResponse{}, errors.New("boom")
					},
					prompt: func(context.Context, acp.PromptRequest) (acp.PromptResponse, error) {
						return acp.PromptResponse{}, acp.NewAuthRequired(nil)
					},
				})
				w.send(tt.line)
				got := w.recv()
				require.JSONEq(t, tt.wantID, string(got.ID))
				require.NotNil(t, got.Error)
				require.Equal(t, tt.wantCode, got.Error.Code)
			})
		})
	}
}

func TestUnknownNotificationIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, w := serveAgent(t, &agent{})

		w.send(
			`{"jsonrpc":"2.0","method":"_example.com/file_opened","params":{"path":"/a"}}`,
			`{"jsonrpc":"2.0","id":1,"method":"_example.com/buffers"}`,
		)

		// The first thing written answers the request: the notification got
		// no reply, and the unknown request got "Method not found".
		got := w.recv()
		require.JSONEq(t, `1`, string(got.ID))
		require.NotNil(t, got.Error)
		require.Equal(t, acp.NewMethodNotFound("").Code, got.Error.Code)
	})
}

func TestNotificationHandlerCannotWaitForPeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag, cl := &agent{}, &client{}
		agentConn, clientConn := connect(t, ag, cl)

		got := make(chan error, 1)
		cl.sessionUpdate = func(ctx context.Context, n acp.SessionNotification) error {
			_, err := clientConn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: n.SessionId, ModeId: "ask"})
			got <- err
			return nil
		}
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			err := agentConn.SessionUpdate(ctx, agentMessage(p.SessionId, "hello"))
			return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, err
		}

		_, err := clientConn.Prompt(t.Context(), acp.PromptRequest{SessionId: "s", Prompt: []acp.ContentBlock{}})
		require.NoError(t, err)
		require.ErrorIs(t, <-got, acp.ErrWaitInNotification)
	})
}

func TestClosedConnectionFailsCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ag := &agent{}
		agentConn, w := serveAgent(t, ag)

		got := make(chan error, 1)
		ag.prompt = func(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
			_, err := agentConn.ReadTextFile(ctx, acp.ReadTextFileRequest{SessionId: p.SessionId, Path: "/a"})
			got <- err
			return acp.PromptResponse{}, err
		}

		w.send(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"s","prompt":[]}}`)
		require.Equal(t, "fs/read_text_file", w.recv().Method)

		// The client goes away without answering.
		w.close()
		require.ErrorIs(t, <-got, acp.ErrClosed)

		<-agentConn.Done()
		_, err := agentConn.ReadTextFile(t.Context(), acp.ReadTextFileRequest{SessionId: "s", Path: "/a"})
		require.ErrorIs(t, err, acp.ErrClosed)
	})
}

// agent is an Agent whose behavior a test sets one method at a time. A method
// the test did not set is a nil call, which fails the test.
type agent struct {
	acp.Agent
	initialize func(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	prompt     func(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	cancel     func(context.Context, acp.CancelNotification) error
}

func (a *agent) Initialize(ctx context.Context, p acp.InitializeRequest) (acp.InitializeResponse, error) {
	return a.initialize(ctx, p)
}

func (a *agent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	return a.prompt(ctx, p)
}

func (a *agent) Cancel(ctx context.Context, p acp.CancelNotification) error {
	return a.cancel(ctx, p)
}

// client is the Client counterpart of agent.
type client struct {
	acp.Client
	sessionUpdate func(context.Context, acp.SessionNotification) error
	readTextFile  func(context.Context, acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error)
	writeTextFile func(context.Context, acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error)
}

func (c *client) WriteTextFile(ctx context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return c.writeTextFile(ctx, p)
}

func (c *client) SessionUpdate(ctx context.Context, p acp.SessionNotification) error {
	return c.sessionUpdate(ctx, p)
}

func (c *client) ReadTextFile(ctx context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return c.readTextFile(ctx, p)
}

func textBlock(text string) acp.ContentBlock {
	return acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: text}}
}

func agentMessage(session acp.SessionId, text string) acp.SessionNotification {
	return acp.SessionNotification{
		SessionId: session,
		Update:    acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: textBlock(text)}},
	}
}

// connect joins an agent and a client with in-memory pipes and serves both
// ends until the test finishes.
func connect(t *testing.T, a acp.Agent, c acp.Client) (*acp.AgentSideConnection, *acp.ClientSideConnection) {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	agentConn := acp.NewAgentSideConnection(a, toClientW, toAgentR, nil)
	clientConn := acp.NewClientSideConnection(c, toAgentW, toClientR, nil)

	var served sync.WaitGroup
	served.Go(func() { _ = agentConn.Serve(context.Background()) })
	served.Go(func() { _ = clientConn.Serve(context.Background()) })
	t.Cleanup(func() {
		for _, end := range []io.Closer{toAgentR, toAgentW, toClientR, toClientW} {
			_ = end.Close()
		}
		served.Wait()
	})
	return agentConn, clientConn
}

// wire is the client's end of a connection, driven one raw line at a time.
type wire struct {
	t   *testing.T
	in  *io.PipeWriter
	out *io.PipeReader
	br  *bufio.Reader
}

type wireMessage struct {
	ID     json.RawMessage   `json:"id"`
	Method string            `json:"method"`
	Error  *acp.RequestError `json:"error"`
}

// serveAgent serves an agent whose client is the returned wire.
func serveAgent(t *testing.T, a acp.Agent) (*acp.AgentSideConnection, *wire) {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	agentConn := acp.NewAgentSideConnection(a, toClientW, toAgentR, nil)
	w := &wire{t: t, in: toAgentW, out: toClientR, br: bufio.NewReader(toClientR)}

	var served sync.WaitGroup
	served.Go(func() { _ = agentConn.Serve(context.Background()) })
	t.Cleanup(func() {
		w.close()
		served.Wait()
	})
	return agentConn, w
}

func (w *wire) send(lines ...string) {
	w.t.Helper()
	_, err := io.WriteString(w.in, strings.Join(lines, "\n")+"\n")
	require.NoError(w.t, err)
}

func (w *wire) recv() wireMessage {
	w.t.Helper()
	line, err := w.br.ReadBytes('\n')
	require.NoError(w.t, err)
	var m wireMessage
	require.NoError(w.t, json.Unmarshal(line, &m), "line: %s", line)
	return m
}

// close ends the client: the agent reads end of input, and its writes fail.
func (w *wire) close() {
	_ = w.in.Close()
	_ = w.out.Close()
}
