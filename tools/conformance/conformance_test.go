package conformance

import (
	"context"
	sdkjson "encoding/json" //nolint:depguard // the SDK's types are coded with the SDK's encoder
	"os"
	"strings"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

// timeout bounds one test's conversation. It is far above what the agent
// needs, so that reaching it means a message was lost and not a slow host.
const timeout = 60 * time.Second

// unknownSession is a session id the agent never issued.
const unknownSession = "no-such-session"

// golden holds what the agent wrote in TestWire, normalized. envUpdate
// makes TestWire write it from the binary under test.
const (
	golden    = "testdata/wire.golden"
	envUpdate = "ACPHARNESS_CONFORMANCE_UPDATE"
)

func initialize(ctx context.Context, t *testing.T, conn *acp.ClientSideConnection) acp.InitializeResponse {
	t.Helper()
	resp, err := conn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientInfo:      &acp.Implementation{Name: "acp-go-sdk conformance", Version: "0.13.5"},
	})
	require.NoError(t, err)
	return resp
}

func newSession(ctx context.Context, t *testing.T, conn *acp.ClientSideConnection) acp.NewSessionResponse {
	t.Helper()
	resp, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.NotEmpty(t, resp.SessionId)
	return resp
}

// requestError requires err to be a JSON-RPC error and returns it.
func requestError(t *testing.T, err error) *acp.RequestError {
	t.Helper()
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	return reqErr
}

// modelSelectionError checks the machine-readable stop, not error wording.
func modelSelectionError(t *testing.T, err error) {
	t.Helper()
	rejected := requestError(t, err)
	require.Equal(t, -32603, rejected.Code)
	data, ok := rejected.Data.(map[string]any)
	require.True(t, ok, "error data: %#v", rejected.Data)
	addition, ok := data["acpharness"].(map[string]any)
	require.True(t, ok, "error data: %#v", data)
	stop, ok := addition["stop"].(map[string]any)
	require.True(t, ok, "harness addition: %#v", addition)
	require.Equal(t, "request", stop["kind"])
}

// kinds names the variant of each update, in the order received.
func kinds(updates []acp.SessionNotification) []string {
	out := []string{}
	for _, n := range updates {
		u := n.Update
		switch {
		case u.AgentMessageChunk != nil:
			out = append(out, "agent_message_chunk")
		case u.AvailableCommandsUpdate != nil:
			out = append(out, "available_commands_update")
		case u.CurrentModeUpdate != nil:
			out = append(out, "current_mode_update")
		case u.ConfigOptionUpdate != nil:
			out = append(out, "config_option_update")
		default:
			raw, _ := sdkjson.Marshal(u)
			out = append(out, "other: "+string(raw))
		}
	}
	return out
}

func TestInitialize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a := start(t)
	conn, _ := a.connect()

	resp := initialize(ctx, t, conn)
	require.Equal(t, acp.ProtocolVersion(acp.ProtocolVersionNumber), resp.ProtocolVersion)
	require.NotNil(t, resp.AgentInfo)
	require.Equal(t, "acpharness", resp.AgentInfo.Name)
	require.NotEmpty(t, resp.AgentInfo.Version)
	require.Empty(t, resp.AuthMethods)

	// What the agent sent that SDK 0.13.5 has no field for: the
	// capabilities of the unstable methods it implements.
	raw := a.lastResult()
	require.Equal(t, []string{
		"agentCapabilities.sessionCapabilities.archive",
		"agentCapabilities.sessionCapabilities.options",
		"agentCapabilities.sessionCapabilities.status",
		"agentCapabilities.sessionCapabilities.systemPrompt",
		"agentCapabilities.sessionCapabilities.title",
	}, notDecoded(t, raw, resp), "raw result: %s", raw)

	// A version the agent does not have is answered with the one it has.
	b := start(t)
	connB, _ := b.connect()
	older, err := connB.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: 9999})
	require.NoError(t, err)
	require.Equal(t, acp.ProtocolVersion(acp.ProtocolVersionNumber), older.ProtocolVersion)
}

func TestNewSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a := start(t)
	conn, c := a.connect()
	initialize(ctx, t, conn)

	resp := newSession(ctx, t, conn)
	require.Empty(t, notDecoded(t, a.lastResult(), resp), "raw result: %s", a.lastResult())

	// Two sessions on one connection are two sessions.
	second := newSession(ctx, t, conn)
	require.NotEqual(t, resp.SessionId, second.SessionId)
	listed, err := conn.ListSessions(ctx, acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Equal(t, []string{"sessions[].archived"}, notDecoded(t, a.lastResult(), listed), "raw result: %s", a.lastResult())
	ids := []acp.SessionId{}
	for _, saved := range listed.Sessions {
		ids = append(ids, saved.SessionId)
		require.DirExists(t, saved.Cwd)
	}
	require.ElementsMatch(t, []acp.SessionId{resp.SessionId, second.SessionId}, ids)
	require.Equal(t, []string{"available_commands_update", "available_commands_update"}, kinds(c.take()))
}

// Methods the agent does not implement answer Method not found: an
// unstable provider method and an extension method nobody registered.
func TestMethodNotFound(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a := start(t)
	conn, _ := a.connect()
	initialize(ctx, t, conn)

	_, err := conn.UnstableListProviders(ctx, acp.UnstableListProvidersRequest{})
	require.Equal(t, -32601, requestError(t, err).Code)

	_, err = conn.CallExtension(ctx, "_conformance/no_such_method", map[string]any{})
	require.Equal(t, -32601, requestError(t, err).Code)

	// An extension notification nobody registered is ignored, and the
	// connection goes on.
	require.NoError(t, conn.NotifyExtension(ctx, "_conformance/no_such_notification", map[string]any{}))
	newSession(ctx, t, conn)
	// Four requests were answered, and the notification was not.
	answers := 0
	for _, m := range a.messages() {
		if m.Method == "" {
			answers++
		}
	}
	require.Equal(t, 4, answers, "initialize, two unknown methods and session/new")
}

// TestWire compares everything the agent writes in one conversation with
// the file recorded from it. It is the check for a change of encoder: v1
// and v2 differ in which empty members are left out and in how a nil list
// is written, and a client sees both. A line is compared as JSON, so the
// order of members and the spacing do not count.
//
// When the agent's output changes on purpose, run the tests once with
// ACPHARNESS_CONFORMANCE_UPDATE=1 and review the diff of the file.
func TestWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	a := start(t)
	conn, _ := a.connect()
	initialize(ctx, t, conn)
	session := newSession(ctx, t, conn)
	// A cancel is a notification: with no turn running and for a session
	// nobody made it is not answered, which the line count below shows, and
	// the connection goes on.
	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}))
	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: unknownSession}))
	_, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	modelSelectionError(t, err)
	_, err = conn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: session.SessionId, ModeId: "ask"})
	require.NoError(t, err)
	_, err = conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{SessionId: session.SessionId, ConfigId: "mode", Value: "auto"},
	})
	require.NoError(t, err)
	listed, err := conn.ListSessions(ctx, acp.ListSessionsRequest{Cwd: new("/acpharness-conformance-no-such-cwd")})
	require.NoError(t, err)
	require.Empty(t, listed.Sessions)
	_, err = conn.NewSession(ctx, acp.NewSessionRequest{})
	require.Error(t, err)
	_, err = conn.SetSessionMode(ctx, acp.SetSessionModeRequest{SessionId: unknownSession, ModeId: "ask"})
	require.Error(t, err)
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	// Every update the agent sent decodes in the SDK without loss.
	sent := 0
	for _, m := range a.messages() {
		if m.Method != acp.ClientMethodSessionUpdate {
			continue
		}
		var n acp.SessionNotification
		require.NoError(t, sdkjson.Unmarshal(m.Params, &n), "raw params: %s", m.Params)
		require.Empty(t, notDecoded(t, m.Params, n), "raw params: %s", m.Params)
		sent++
	}
	require.Positive(t, sent)

	got := a.normalized()
	if os.Getenv(envUpdate) != "" {
		require.NoError(t, os.MkdirAll("testdata", 0o755))
		require.NoError(t, os.WriteFile(golden, []byte(strings.Join(got, "\n")+"\n"), 0o644))
		return
	}
	recorded, err := os.ReadFile(golden)
	require.NoError(t, err)
	want := strings.Split(strings.TrimSuffix(string(recorded), "\n"), "\n")
	require.Len(t, got, len(want), "the agent wrote:\n%s", strings.Join(got, "\n"))
	for i := range want {
		require.JSONEq(t, want[i], got[i], "line %d", i+1)
	}
}
