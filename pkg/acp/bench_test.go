package acp_test

import (
	"context"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
)

// The benchmarks go through a connection, so they count everything a message
// costs: the frame, the envelope, the typed params and the handler dispatch.
// Run with: go test ./pkg/acp -run '^$' -bench . -benchmem

// benchText is the text of a streamed chunk: a few lines of source code.
const benchText = "func (c *Connection) Done() <-chan struct{} { return c.done }\n\n// Serve handles messages until the reader ends.\n"

// benchPrompt is a prompt with a short instruction and one embedded file.
func benchPrompt() acp.PromptRequest {
	return acp.PromptRequest{
		SessionId: "0196f3a2-7c1e-7b7e-9d55-3f1c2a9b8e10",
		Prompt: []acp.ContentBlock{
			textBlock("Explain what this file does and fix the bug in Serve."),
			{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
				TextResourceContents: &acp.TextResourceContents{
					Uri:  "file:///home/user/project/connection.go",
					Text: strings.Repeat(benchText, 64),
				},
			}}},
		},
	}
}

func benchUpdate() acp.SessionNotification {
	return agentMessage("0196f3a2-7c1e-7b7e-9d55-3f1c2a9b8e10", benchText)
}

// encodeLine returns the line a connection writes for one notification.
func encodeLine(tb testing.TB, method string, params any) string {
	tb.Helper()
	var out strings.Builder
	conn := acp.NewConnection(nil, &out, strings.NewReader(""), nil)
	require.NoError(tb, conn.SendNotification(context.Background(), method, params))
	return out.String()
}

// repeated reads line n times, then ends.
type repeated struct {
	line string
	n    int
	rest string
}

func (r *repeated) Read(p []byte) (int, error) {
	if r.rest == "" {
		if r.n == 0 {
			return 0, io.EOF
		}
		r.n--
		r.rest = r.line
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func benchEncode(b *testing.B, method string, params any) {
	conn := acp.NewConnection(nil, io.Discard, strings.NewReader(""), nil)
	b.SetBytes(int64(len(encodeLine(b, method, params))))
	b.ReportAllocs()
	for b.Loop() {
		if err := conn.SendNotification(context.Background(), method, params); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeSessionUpdate(b *testing.B) {
	benchEncode(b, acp.ClientMethodSessionUpdate, benchUpdate())
}

// The prompt is written as a notification so that nothing has to answer it.
// The message differs from a request only by its id.
func BenchmarkEncodePromptRequest(b *testing.B) {
	benchEncode(b, acp.AgentMethodSessionPrompt, benchPrompt())
}

// BenchmarkDecodeSessionUpdate is a client reading session/update
// notifications and handing each to its handler.
func BenchmarkDecodeSessionUpdate(b *testing.B) {
	line := encodeLine(b, acp.ClientMethodSessionUpdate, benchUpdate())
	seen := 0
	cl := &client{sessionUpdate: func(_ context.Context, n acp.SessionNotification) error {
		if n.Update.AgentMessageChunk != nil && n.Update.AgentMessageChunk.Content.Text != nil && n.Update.AgentMessageChunk.Content.Text.Text == benchText {
			seen++
		}
		return nil
	}}
	conn := acp.NewClientSideConnection(cl, io.Discard, &repeated{line: line, n: b.N}, nil)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	if err := conn.Serve(context.Background()); err != nil {
		b.Fatal(err)
	}
	if seen != b.N {
		b.Fatalf("handled %d of %d", seen, b.N)
	}
}

// BenchmarkDecodePromptRequest is an agent reading session/prompt requests,
// handling each on its own goroutine and writing the response.
func BenchmarkDecodePromptRequest(b *testing.B) {
	line := encodeLine(b, acp.AgentMethodSessionPrompt, benchPrompt())
	var seen atomic.Int64
	ag := &agent{prompt: func(_ context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
		if len(p.Prompt) == 2 && p.Prompt[1].Resource != nil && p.Prompt[1].Resource.Resource.TextResourceContents != nil && len(p.Prompt[1].Resource.Resource.TextResourceContents.Text) == 64*len(benchText) {
			seen.Add(1)
		}
		return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
	}}
	conn := acp.NewAgentSideConnection(ag, io.Discard, newNumbered(b, line, b.N), nil)
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	b.ResetTimer()
	if err := conn.Serve(context.Background()); err != nil {
		b.Fatal(err)
	}
	if seen.Load() != int64(b.N) {
		b.Fatalf("handled %d of %d", seen.Load(), b.N)
	}
}

// numbered reads line n times with a different request id each time, since a
// connection refuses an id that is still in use. The id is a fixed-width
// string written in place, so the reader allocates nothing.
type numbered struct {
	line []byte
	id   []byte
	n    int
	rest []byte
}

func newNumbered(tb testing.TB, line string, n int) *numbered {
	tb.Helper()
	const mark = `{"jsonrpc":"2.0",`
	require.True(tb, strings.HasPrefix(line, mark))
	prefix := mark + `"id":"`
	idWidth := len(strconv.Itoa(n))
	b := []byte(prefix + strings.Repeat("0", idWidth) + `",` + line[len(mark):])
	return &numbered{line: b, id: b[len(prefix) : len(prefix)+idWidth], n: n}
}

func (r *numbered) Read(p []byte) (int, error) {
	if len(r.rest) == 0 {
		if r.n == 0 {
			return 0, io.EOF
		}
		for i, v := len(r.id)-1, r.n; i >= 0; i, v = i-1, v/10 {
			r.id[i] = byte('0' + v%10)
		}
		r.n--
		r.rest = r.line
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}
