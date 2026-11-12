// Package acptest drives an ACP agent from a test, one raw JSON line at a
// time, as a client that this repository did not write would.
package acptest

import (
	"bufio"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
)

// Wire is the client's end of a connection to an agent.
type Wire struct {
	t   testing.TB
	in  *io.PipeWriter
	out *io.PipeReader
	br  *bufio.Reader
}

// Message is one JSON-RPC message from the agent.
type Message struct {
	ID     jsontext.Value    `json:"id"`
	Method string            `json:"method"`
	Params jsontext.Value    `json:"params"`
	Result jsontext.Value    `json:"result"`
	Error  *acp.RequestError `json:"error"`
}

// New returns a wire and the agent's ends of it: what the agent reads its
// input from and writes its output to. The wire is closed when the test ends.
func New(t testing.TB) (w *Wire, agentIn io.Reader, agentOut io.Writer) {
	t.Helper()
	toAgentR, toAgentW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	w = &Wire{t: t, in: toAgentW, out: toClientR, br: bufio.NewReader(toClientR)}
	t.Cleanup(w.Close)
	return w, toAgentR, toClientW
}

// Send writes the lines to the agent in one write, so the agent can read
// them all before it handles the first.
func (w *Wire) Send(lines ...string) {
	w.t.Helper()
	_, err := io.WriteString(w.in, strings.Join(lines, "\n")+"\n")
	require.NoError(w.t, err)
}

// Recv reads the agent's next message.
func (w *Wire) Recv() Message {
	w.t.Helper()
	line, err := w.br.ReadBytes('\n')
	require.NoError(w.t, err)
	var m Message
	require.NoError(w.t, json.Unmarshal(line, &m), "line: %s", line)
	return m
}

// Await reads until the agent has answered every request in ids, which are
// JSON ids such as `1` or `"a"`. It returns the responses by id and, in the
// order they were read, the notifications and requests that came with them.
func (w *Wire) Await(ids ...string) (responses map[string]Message, others []Message) {
	w.t.Helper()
	responses = map[string]Message{}
	for len(responses) < len(ids) {
		m := w.Recv()
		if m.Method != "" {
			others = append(others, m)
			continue
		}
		id := string(m.ID)
		require.Contains(w.t, ids, id, "response to a request that was not awaited")
		require.NotContains(w.t, responses, id, "second response to one request")
		responses[id] = m
	}
	return responses, others
}

// Close ends the client: the agent reads end of input and its writes fail.
func (w *Wire) Close() {
	_ = w.in.Close()
	_ = w.out.Close()
}
