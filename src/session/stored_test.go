package session_test

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/acptest"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// envStoreRoot makes the test binary serve an agent on standard input and
// output over the store in the named directory, in place of running tests.
// TestSessionMovesToANewProcess starts the binary that way twice.
const envStoreRoot = "ACPHARNESS_SESSION_TEST_STORE_ROOT"

func TestMain(m *testing.M) {
	root := os.Getenv(envStoreRoot)
	if root == "" {
		os.Exit(m.Run())
	}
	st, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionZstd})
	if err != nil {
		panic(err)
	}
	options := agentOptions(storedTurn(st))
	options.Store = st
	agent, err := session.NewAgent(options)
	if err != nil {
		panic(err)
	}
	conn := acp.NewAgentSideConnection(agent, os.Stdout, os.Stdin, nil)
	agent.SetClient(conn)
	if err := conn.Serve(context.Background()); err != nil {
		panic(err)
	}
	os.Exit(0)
}

func agentOptions(turn session.TurnFunc) session.Options {
	return session.Options{
		Version: "test",
		Turn:    turn,
		Logger:  slog.New(slog.DiscardHandler),
		Tracer:  tracenoop.NewTracerProvider().Tracer("test"),
		Meter:   metricnoop.NewMeterProvider().Meter("test"),
		Session: config.Session{Mode: config.SessionModeAuto},
	}
}

// storedTurn is a turn that does what the loop does with the store and the
// mapper, without a model: it appends the prompt and a fixed answer (a
// thought, text, a tool call and its result) to the session, and shows the
// answer to the client through the same mapper a replay uses. A prompt of
// "compact" first records a compaction, as a turn that ran out of context
// would.
func storedTurn(st store.Store) session.TurnFunc {
	return func(ctx context.Context, turn loop.Turn) (loop.Result, error) {
		id, err := uuid.Parse(string(turn.SessionID))
		if err != nil {
			return loop.Result{}, err
		}
		user, err := loop.UserItem(turn.Prompt)
		if err != nil {
			return loop.Result{}, err
		}
		text := turn.Prompt[0].Text.Text
		var hidden []transcript.Item
		if text == "compact" {
			hidden = append(hidden, transcript.Item{Compaction: &transcript.Compaction{
				Summary: "what came before", FirstKept: uuid.NewV7(), Mode: "reactive",
			}})
		}
		answer := []transcript.Item{
			{Thinking: &transcript.Thinking{Text: "thinking about " + text}},
			{Text: &transcript.Text{Text: "reply to " + text}},
			{ToolCall: &transcript.ToolCall{ID: "call-" + text, Name: "read", Input: jsontext.Value(`{"path":"a.go"}`)}},
			{ToolResult: &transcript.ToolResult{
				CallID: "call-" + text, Name: "read", Outcome: transcript.OutcomeCompleted,
				Parts: []transcript.Part{{Text: "package a"}},
			}},
		}
		var entries []transcript.Entry
		for _, item := range append(append(hidden, user), answer...) {
			entries = append(entries, transcript.NewEntry(uuid.Nil(), time.Now(), item))
		}
		if err := st.Append(ctx, id, entries); err != nil {
			return loop.Result{}, err
		}
		for _, item := range answer {
			updates, err := loop.Mapper{}.Updates(item)
			if err != nil {
				return loop.Result{}, err
			}
			for _, update := range updates {
				if err := turn.Update(ctx, update); err != nil {
					return loop.Result{}, err
				}
			}
		}
		return loop.Result{StopReason: acp.StopReasonEndTurn}, nil
	}
}

// pod is one agent over the store in root, with its own store value, as a
// process of its own would have.
func pod(t *testing.T, root string, adjust func(*session.Options)) *acptest.Wire {
	t.Helper()
	st, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionZstd})
	require.NoError(t, err)
	return serveWith(t, storedTurn(st), func(o *session.Options) {
		o.Store = st
		if adjust != nil {
			adjust(o)
		}
	})
}

// shown returns the updates that carry conversation content, as JSON, and
// the others.
func shown(t *testing.T, messages []acptest.Message) (content []string, others []acp.SessionUpdate) {
	t.Helper()
	for _, n := range updatesOf(t, messages) {
		u := n.Update
		if u.UserMessageChunk == nil && u.AgentMessageChunk == nil && u.AgentThoughtChunk == nil && u.ToolCall == nil && u.ToolCallUpdate == nil {
			others = append(others, u)
			continue
		}
		encoded, err := json.Marshal(u)
		require.NoError(t, err)
		content = append(content, string(encoded))
	}
	return content, others
}

func userChunk(t *testing.T, text string) string {
	t.Helper()
	encoded, err := json.Marshal(acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{
		Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: text}},
	}})
	require.NoError(t, err)
	return string(encoded)
}

func loadRequest(id, sessionID, cwd string) string {
	return request(id, "session/load", `{"sessionId":"`+sessionID+`","cwd":"`+cwd+`","mcpServers":[]}`)
}

func TestStoredSessionIsLoadedByAnotherAgent(t *testing.T) {
	root := t.TempDir()
	first := pod(t, root, nil)

	// With a store the methods that work are advertised.
	first.Send(request(`"init"`, "initialize", `{"protocolVersion":1}`))
	got, _ := first.Await(`"init"`)
	var init acp.InitializeResponse
	require.NoError(t, json.Unmarshal(got[`"init"`].Result, &init))
	require.True(t, init.AgentCapabilities.LoadSession)
	sessions := init.AgentCapabilities.SessionCapabilities
	require.NotNil(t, sessions.Resume)
	require.NotNil(t, sessions.List)
	require.NotNil(t, sessions.Delete)
	require.NotNil(t, sessions.Fork)
	require.NotNil(t, sessions.Archive)

	first.Send(newSession)
	got, _ = first.Await(`"new"`)
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(got[`"new"`].Result, &created))
	id := string(created.SessionId)

	// Two turns on the first agent, with a title and a mode set between
	// them. The second turn records a compaction.
	first.Send(prompt(`1`, id, "first"))
	_, others := first.Await(`1`)
	live, _ := shown(t, others)
	require.Len(t, live, 4)
	first.Send(
		request(`2`, "session/set_title", `{"sessionId":"`+id+`","title":"Fix the parser"}`),
		request(`3`, "session/set_mode", `{"sessionId":"`+id+`","modeId":"ask"}`),
	)
	first.Await(`2`, `3`)
	first.Send(prompt(`4`, id, "compact"))
	_, others = first.Await(`4`)
	second, _ := shown(t, others)
	want := append(append(append([]string{userChunk(t, "first")}, live...), userChunk(t, "compact")), second...)

	// The new pod: another agent with its own store value on the directory.
	next := pod(t, root, nil)
	next.Send(
		initialize,
		request(`1`, "session/status", `{"sessionId":"`+id+`"}`),
		request(`2`, "session/list", `{}`),
		request(`3`, "session/list", `{"cwd":"/elsewhere"}`),
		loadRequest(`4`, id, "/elsewhere"),
		loadRequest(`5`, uuid.NewV7().String(), "/work"),
		loadRequest(`6`, "not-an-id", "/work"),
		prompt(`7`, id, "too early"),
	)
	got, others = next.Await(`"init"`, `1`, `2`, `3`, `4`, `5`, `6`, `7`)
	require.Empty(t, others)
	require.JSONEq(t, `{"status":"saved"}`, string(got[`1`].Result))
	var listed acp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(got[`2`].Result, &listed))
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, id, string(listed.Sessions[0].SessionId))
	require.Equal(t, "/work", listed.Sessions[0].Cwd)
	require.Equal(t, "Fix the parser", *listed.Sessions[0].Title)
	require.Nil(t, listed.NextCursor)
	require.JSONEq(t, `{"sessions":[]}`, string(got[`3`].Result))
	require.Equal(t, acp.NewInvalidParams(nil).Code, got[`4`].Error.Code)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`5`].Error.Code)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`6`].Error.Code)
	// A stored session is not live until it is loaded or resumed.
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`7`].Error.Code)

	next.Send(loadRequest(`8`, id, "/work"))
	got, others = next.Await(`8`)
	require.Nil(t, got[`8`].Error)
	// The whole history in stored order, the turn before the compaction
	// included, as the first agent's client saw it, with the prompts.
	replayed, rest := shown(t, others)
	require.Equal(t, want, replayed)
	// Then the stored title and the commands, all before the answer.
	require.Len(t, rest, 2)
	require.Equal(t, "Fix the parser", *rest[0].SessionInfoUpdate.Title)
	require.NotNil(t, rest[1].AvailableCommandsUpdate)
	var loaded acp.LoadSessionResponse
	require.NoError(t, json.Unmarshal(got[`8`].Result, &loaded))
	require.Equal(t, acp.SessionModeId("ask"), loaded.Modes.CurrentModeId)

	// The session is live here now, and a prompt continues it.
	next.Send(request(`9`, "session/status", `{"sessionId":"`+id+`"}`), prompt(`10`, id, "third"))
	got, others = next.Await(`9`, `10`)
	require.JSONEq(t, `{"status":"open"}`, string(got[`9`].Result))
	require.JSONEq(t, endTurn, string(got[`10`].Result))
	third, _ := shown(t, others)
	require.Len(t, third, 4)

	// Loading a session that is live here starts it again from the store.
	next.Send(loadRequest(`11`, id, "/work"))
	got, others = next.Await(`11`)
	require.Nil(t, got[`11`].Error)
	replayed, _ = shown(t, others)
	require.Equal(t, append(append(want, userChunk(t, "third")), third...), replayed)

	// Deleted on one agent, gone for both.
	next.Send(request(`12`, "session/delete", `{"sessionId":"`+id+`"}`))
	got, _ = next.Await(`12`)
	require.JSONEq(t, `{}`, string(got[`12`].Result))
	for _, w := range []*acptest.Wire{next, first} {
		w.Send(request(`13`, "session/list", `{}`), loadRequest(`14`, id, "/work"), request(`15`, "session/delete", `{"sessionId":"`+id+`"}`))
		got, _ = w.Await(`13`, `14`, `15`)
		require.JSONEq(t, `{"sessions":[]}`, string(got[`13`].Result))
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`14`].Error.Code)
		require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`15`].Error.Code)
	}
	next.Send(request(`16`, "session/status", `{"sessionId":"`+id+`"}`))
	got, _ = next.Await(`16`)
	require.Equal(t, acp.NewResourceNotFound(nil).Code, got[`16`].Error.Code)
}

func TestResumeTitleASavedSessionAndListInPages(t *testing.T) {
	root := t.TempDir()
	first := pod(t, root, nil)
	first.Send(initialize)
	first.Await(`"init"`)
	var ids []string
	for range 3 {
		first.Send(newSession)
		got, _ := first.Await(`"new"`)
		var created acp.NewSessionResponse
		require.NoError(t, json.Unmarshal(got[`"new"`].Result, &created))
		ids = append(ids, string(created.SessionId))
	}
	first.Send(prompt(`1`, ids[0], "first"))
	first.Await(`1`)

	next := pod(t, root, func(o *session.Options) { o.Session.ListPageSize = 2 })
	// A session in the store and not in memory can be titled.
	next.Send(initialize, request(`1`, "session/set_title", `{"sessionId":"`+ids[1]+`","title":" Second "}`))
	got, others := next.Await(`"init"`, `1`)
	require.JSONEq(t, `{"title":"Second"}`, string(got[`1`].Result))
	require.Empty(t, others)

	// Two pages, oldest first, and the cursor ends with the last page.
	next.Send(request(`2`, "session/list", `{}`))
	got, _ = next.Await(`2`)
	var page acp.ListSessionsResponse
	require.NoError(t, json.Unmarshal(got[`2`].Result, &page))
	require.Len(t, page.Sessions, 2)
	require.Equal(t, ids[0], string(page.Sessions[0].SessionId))
	require.Equal(t, "Second", *page.Sessions[1].Title)
	require.NotNil(t, page.NextCursor)
	next.Send(
		request(`3`, "session/list", `{"cursor":"`+*page.NextCursor+`"}`),
		request(`4`, "session/list", `{"cursor":"page-2"}`),
	)
	got, _ = next.Await(`3`, `4`)
	page = acp.ListSessionsResponse{}
	require.NoError(t, json.Unmarshal(got[`3`].Result, &page))
	require.Len(t, page.Sessions, 1)
	require.Equal(t, ids[2], string(page.Sessions[0].SessionId))
	require.Nil(t, page.NextCursor)
	require.Equal(t, acp.NewInvalidParams(nil).Code, got[`4`].Error.Code)

	// Resume replays nothing, and the session then takes a prompt.
	next.Send(request(`5`, "session/resume", `{"sessionId":"`+ids[0]+`","cwd":"/work"}`))
	got, others = next.Await(`5`)
	require.Nil(t, got[`5`].Error)
	replayed, rest := shown(t, others)
	require.Empty(t, replayed)
	require.Len(t, rest, 2)
	next.Send(prompt(`6`, ids[0], "second"))
	got, _ = next.Await(`6`)
	require.JSONEq(t, endTurn, string(got[`6`].Result))
}

// process is an agent in a process of its own: the test binary, started
// with envStoreRoot set.
type process struct {
	t     *testing.T
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *bufio.Reader
}

func startProcess(t *testing.T, root string) *process {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), envStoreRoot+"="+root)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	return &process{t: t, cmd: cmd, stdin: stdin, out: bufio.NewReader(stdout)}
}

// call sends one request and reads until its response, which it returns
// with the messages that came before it.
func (p *process) call(id, line string) (acptest.Message, []acptest.Message) {
	p.t.Helper()
	_, err := io.WriteString(p.stdin, line+"\n")
	require.NoError(p.t, err)
	var before []acptest.Message
	for {
		raw, err := p.out.ReadBytes('\n')
		require.NoError(p.t, err)
		var m acptest.Message
		require.NoError(p.t, json.Unmarshal(raw, &m))
		if m.Method == "" {
			require.Equal(p.t, id, string(m.ID))
			return m, before
		}
		before = append(before, m)
	}
}

// exit ends the process as a pod that is drained does: its input closes.
func (p *process) exit() {
	p.t.Helper()
	require.NoError(p.t, p.stdin.Close())
	require.NoError(p.t, p.cmd.Wait())
}

// The claim itself: a process runs a session and exits, and a new process
// that is given the same directory loads the session and carries on.
func TestSessionMovesToANewProcess(t *testing.T) {
	root := t.TempDir()

	old := startProcess(t, root)
	old.call(`"init"`, initialize)
	made, _ := old.call(`"new"`, newSession)
	var created acp.NewSessionResponse
	require.NoError(t, json.Unmarshal(made.Result, &created))
	id := string(created.SessionId)
	answer, sent := old.call(`1`, prompt(`1`, id, "first"))
	require.JSONEq(t, endTurn, string(answer.Result))
	live, _ := shown(t, sent)
	require.Len(t, live, 4)
	titled, _ := old.call(`2`, request(`2`, "session/set_title", `{"sessionId":"`+id+`","title":"Moved"}`))
	require.Nil(t, titled.Error)
	old.exit()

	fresh := startProcess(t, root)
	fresh.call(`"init"`, initialize)
	status, _ := fresh.call(`1`, request(`1`, "session/status", `{"sessionId":"`+id+`"}`))
	require.JSONEq(t, `{"status":"saved"}`, string(status.Result))
	loaded, sent := fresh.call(`2`, loadRequest(`2`, id, "/work"))
	require.Nil(t, loaded.Error)
	replayed, rest := shown(t, sent)
	require.Equal(t, append([]string{userChunk(t, "first")}, live...), replayed)
	require.Equal(t, "Moved", *rest[0].SessionInfoUpdate.Title)

	answer, sent = fresh.call(`3`, prompt(`3`, id, "second"))
	require.JSONEq(t, endTurn, string(answer.Result))
	continued, _ := shown(t, sent)
	require.Len(t, continued, 4)
	fresh.exit()
}

func TestTornSessionMovesToANewProcess(t *testing.T) {
	for _, method := range []string{"session/load", "session/resume"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			old := startProcess(t, root)
			old.call(`"init"`, initialize)
			made, _ := old.call(`"new"`, newSession)
			var created acp.NewSessionResponse
			require.NoError(t, json.Unmarshal(made.Result, &created))
			id := string(created.SessionId)
			answer, sent := old.call(`1`, prompt(`1`, id, "first"))
			require.JSONEq(t, endTurn, string(answer.Result))
			live, _ := shown(t, sent)
			path := filepath.Join(root, id, "log.jsonl.zst")
			whole, err := os.Stat(path)
			require.NoError(t, err)
			old.call(`2`, prompt(`2`, id, "cut short"))
			old.exit()
			full, err := os.Stat(path)
			require.NoError(t, err)
			// Cut inside the real compressed append, not inside a hand-built
			// log. The first turn's appends are left whole.
			cut := whole.Size() + (full.Size()-whole.Size())/2
			require.NoError(t, os.Truncate(path, cut))

			fresh := startProcess(t, root)
			fresh.call(`"init"`, initialize)
			loaded, sent := fresh.call(`1`, request(`1`, method,
				`{"sessionId":"`+id+`","cwd":"/work","mcpServers":[]}`))
			require.Nil(t, loaded.Error)
			var response struct {
				Meta struct {
					Harness struct {
						Repair struct {
							Bytes int64 `json:"setAsideBytes"`
						} `json:"repair"`
					} `json:"acpharness"`
				} `json:"_meta"`
			}
			require.NoError(t, json.Unmarshal(loaded.Result, &response))
			require.Equal(t, cut-whole.Size(), response.Meta.Harness.Repair.Bytes)
			replayed, _ := shown(t, sent)
			if method == "session/load" {
				require.Equal(t, append([]string{userChunk(t, "first")}, live...), replayed)
			} else {
				require.Empty(t, replayed)
			}
			answer, _ = fresh.call(`2`, prompt(`2`, id, "after repair"))
			require.JSONEq(t, endTurn, string(answer.Result))
			fresh.exit()
		})
	}
}
