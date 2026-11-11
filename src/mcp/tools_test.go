package mcp

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func boolPtr(b bool) *bool { return &b }

func noEnv(string) (string, bool) { return "", false }

func TestSessionServersWinByName(t *testing.T) {
	cfg := defaultConfig(t)
	cfg.Servers = []config.MCPServer{
		{Name: "linear", Transport: config.MCPTransportStreamableHTTP, URL: "https://config.example/mcp", HeadersEnv: map[string]string{"X-Key": "LINEAR_KEY"}},
		{Name: "files", Transport: config.MCPTransportStdio, Command: "/bin/files", Env: map[string]string{"B": "2", "A": "1"}, Direct: boolPtr(true)},
	}
	env := func(name string) (string, bool) {
		if name == "LINEAR_KEY" {
			return "secret", true
		}
		return "", false
	}
	session := []acp.McpServer{
		{Stdio: &acp.McpServerStdio{Name: "worker", Command: "/bin/worker", Args: []string{"--x"}, Env: []acp.EnvVariable{{Name: "K", Value: "v"}}}},
		{Sse: &acp.McpServerSseInline{Name: "linear", Url: "https://session.example/sse", Headers: []acp.HttpHeader{{Name: "X-S", Value: "1"}}}},
	}
	got, err := Servers(cfg, session, env)
	require.NoError(t, err)
	require.Equal(t, []Server{
		// The session's definition replaces the configured one whole: its
		// transport, URL and headers, and the configured header is gone.
		{
			Name: "linear", Transport: config.MCPTransportSSE, URL: "https://session.example/sse",
			Headers: map[string]string{"X-S": "1"}, Direct: true, TrustAnnotations: true, FromSession: true,
		},
		{Name: "files", Transport: config.MCPTransportStdio, Command: "/bin/files", Env: []string{"A=1", "B=2"}, Direct: true},
		{
			Name: "worker", Transport: config.MCPTransportStdio, Command: "/bin/worker", Args: []string{"--x"},
			Env: []string{"K=v"}, Direct: true, TrustAnnotations: true, FromSession: true,
		},
	}, got)

	// Without a session the configured server is behind the mcp tool and
	// carries the header read from the environment.
	got, err = Servers(cfg, nil, env)
	require.NoError(t, err)
	require.False(t, got[0].Direct)
	require.Equal(t, map[string]string{"X-Key": "secret"}, got[0].Headers)

	// Configuration can put a session's servers behind the mcp tool.
	cfg.SessionServersDirect = false
	got, err = Servers(cfg, session, env)
	require.NoError(t, err)
	require.False(t, got[2].Direct)
}

func TestServersThatCannotBeUsedAreErrors(t *testing.T) {
	cfg := defaultConfig(t)
	for name, session := range map[string][]acp.McpServer{
		"acp transport":   {{Acp: &acp.McpServerAcpInline{Name: "a"}}},
		"no transport":    {{}},
		"no name":         {{Stdio: &acp.McpServerStdio{Command: "/bin/x"}}},
		"no command":      {{Stdio: &acp.McpServerStdio{Name: "a"}}},
		"not a URL":       {{Http: &acp.McpServerHttpInline{Name: "a", Url: "mcp.example"}}},
		"same name twice": {{Stdio: &acp.McpServerStdio{Name: "a", Command: "/bin/x"}}, {Stdio: &acp.McpServerStdio{Name: "a", Command: "/bin/y"}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Servers(cfg, session, noEnv)
			require.Error(t, err)
		})
	}
	cfg.Servers = []config.MCPServer{{Name: "s", Transport: config.MCPTransportSSE, URL: "https://x.example/", HeadersEnv: map[string]string{"X-Key": "UNSET"}}}
	_, err := Servers(cfg, nil, noEnv)
	require.Error(t, err, "a header whose variable is not set is not sent empty")
}

func run(t *testing.T, tool tools.Tool, input string) tools.Result {
	t.Helper()
	_, err := tool.Describe(jsontext.Value(input))
	require.NoError(t, err)
	res, err := tool.Run(t.Context(), tools.Call{ID: "c1", Input: jsontext.Value(input)})
	require.NoError(t, err)
	return res
}

func text(t *testing.T, res tools.Result) string {
	t.Helper()
	require.NotEmpty(t, res.Parts)
	return res.Parts[0].Text
}

// TestDirectAndMetaTools runs a session with one server from each source: a
// configured remote server behind the mcp tool, and a stdio server the
// session named, whose tools are direct.
func TestDirectAndMetaTools(t *testing.T) {
	remote := startHTTPServer(t, config.MCPTransportStreamableHTTP, nil)
	cfg := defaultConfig(t)
	cfg.SearchResults = 2
	cfg.Servers = []config.MCPServer{{Name: "remote", Transport: config.MCPTransportStreamableHTTP, URL: remote.URL}}
	worker := stdioServer(t, "worker", modeServe)
	servers, err := Servers(cfg, []acp.McpServer{{Stdio: &acp.McpServerStdio{
		Name: worker.Name, Command: worker.Command, Env: []acp.EnvVariable{{Name: serverModeEnv, Value: modeServe}},
	}}}, noEnv)
	require.NoError(t, err)
	m := newTestManager(t, Options{Config: cfg, Servers: servers, HTTPClient: newCountingClient().Client})

	set := tools.Set{Dialect: "generic"}
	set.Tools, err = m.Tools(t.Context(), []string{"read", "shell"})
	require.NoError(t, err)
	names := make([]string, len(set.Tools))
	for i, tool := range set.Tools {
		names[i] = tool.Spec.Name
	}
	require.ElementsMatch(t, []string{"echo", "fail", "picture", "pids", "submit_result", MetaToolName}, names)
	require.Equal(t, MetaToolName, names[len(names)-1])
	require.False(t, m.Connected("remote"), "a server behind the mcp tool is not started to build the tools")

	// A direct tool has the server's own schema and description.
	submit, ok := set.Lookup("submit_result")
	require.True(t, ok)
	require.Equal(t, "Hand in the result of the task.", submit.Spec.Description)
	var schema struct {
		Type       string                    `json:"type"`
		Properties map[string]jsontext.Value `json:"properties"`
		Required   []string                  `json:"required"`
	}
	require.NoError(t, json.Unmarshal(submit.Spec.InputSchema, &schema))
	require.Equal(t, "object", schema.Type)
	require.Contains(t, schema.Properties, "text")
	require.Equal(t, []string{"text"}, schema.Required)

	res := run(t, submit, `{"text":"done"}`)
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Equal(t, "accepted: done", text(t, res))

	// Only a tool that says it is read-only, on a server whose word is
	// taken, may run beside other calls.
	echo, _ := set.Lookup("echo")
	require.Equal(t, tools.ReadOnly, echo.Effect)
	require.Equal(t, tools.Executes, submit.Effect)

	fail, _ := set.Lookup("fail")
	res = run(t, fail, `{}`)
	require.Equal(t, transcript.OutcomeFailed, res.Outcome)
	require.Contains(t, text(t, res), "the disk is full")

	picture, _ := set.Lookup("picture")
	res = run(t, picture, `{}`)
	require.Equal(t, []transcript.Part{
		{Text: "one pixel"},
		{Image: &transcript.Blob{MimeType: "image/png", Data: onePixelPNG}},
	}, res.Parts)

	meta, _ := set.Lookup(MetaToolName)
	require.Equal(t, tools.Executes, meta.Effect)

	// list names only the servers behind the mcp tool.
	var listed struct {
		Servers []listedServer `json:"servers"`
	}
	require.NoError(t, json.Unmarshal([]byte(text(t, run(t, meta, `{"action":"list"}`))), &listed))
	require.Equal(t, []listedServer{{Name: "remote", Transport: config.MCPTransportStreamableHTTP}}, listed.Servers)

	// search returns schemas for the matches, a name match first, and only
	// names beyond mcp.search_results.
	var found searchOutput
	require.NoError(t, json.Unmarshal([]byte(text(t, run(t, meta, `{"action":"search","query":"RESULT"}`))), &found))
	require.Len(t, found.Tools, 1)
	require.Equal(t, "submit_result", found.Tools[0].Name)
	require.Equal(t, "remote", found.Tools[0].Server)
	require.NotNil(t, found.Tools[0].InputSchema)

	found = searchOutput{}
	require.NoError(t, json.Unmarshal([]byte(text(t, run(t, meta, `{"action":"search","query":"","server":"remote"}`))), &found))
	require.Len(t, found.Tools, 2)
	require.Len(t, found.More, 3)

	found = searchOutput{}
	require.NoError(t, json.Unmarshal([]byte(text(t, run(t, meta, `{"action":"search","query":"pixel echo"}`))), &found))
	require.Empty(t, found.Tools, "every word has to match")

	res = run(t, meta, `{"action":"call","server":"remote","tool":"echo","arguments":{"text":"through"}}`)
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Equal(t, "through", text(t, res))

	// What the model gets wrong comes back as a failed call it can read.
	for _, input := range []string{
		`{"action":"call","server":"worker","tool":"echo"}`, // direct, so not behind the mcp tool
		`{"action":"call","server":"remote"}`,
		`{"action":"call","server":"remote","tool":"echo","arguments":[1]}`,
		`{"action":"call","server":"remote","tool":"no_such_tool"}`,
		`{"action":"remove"}`,
	} {
		res = run(t, meta, input)
		require.Equal(t, transcript.OutcomeFailed, res.Outcome, input)
		require.NotEmpty(t, text(t, res), input)
	}
}

func TestSearchReportsAServerItCannotReach(t *testing.T) {
	up := startHTTPServer(t, config.MCPTransportSSE, nil)
	m := newTestManager(t, Options{HTTPClient: newCountingClient().Client, Servers: []Server{
		{Name: "up", Transport: config.MCPTransportSSE, URL: up.URL},
		{Name: "down", Transport: config.MCPTransportStdio, Command: t.TempDir() + "/missing"},
	}})
	list, err := m.Tools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	var found searchOutput
	require.NoError(t, json.Unmarshal([]byte(text(t, run(t, list[0], `{"action":"search","query":"echo"}`))), &found))
	require.Len(t, found.Tools, 1)
	require.Len(t, found.Unavailable, 1)
	require.Equal(t, "down", found.Unavailable[0].Server)
}

func TestDirectToolNameCollisionIsAnError(t *testing.T) {
	a, b := stdioServer(t, "a", modeServe), stdioServer(t, "b", modeServe)
	a.Direct, b.Direct = true, true

	// Two direct servers with the same tools.
	m := newTestManager(t, Options{Servers: []Server{a, b}})
	_, err := m.Tools(t.Context(), nil)
	var collision *NameCollisionError
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "b", collision.Server)
	require.Equal(t, "a", collision.With)

	// A direct tool with the name of one of the session's own tools.
	m = newTestManager(t, Options{Servers: []Server{a}})
	_, err = m.Tools(t.Context(), []string{"read", "echo"})
	collision = nil
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "echo", collision.Name)
	require.Empty(t, collision.With)

	// The same two servers do not collide when one is behind the mcp tool.
	b.Direct = false
	m = newTestManager(t, Options{Servers: []Server{a, b}})
	_, err = m.Tools(t.Context(), []string{"read"})
	require.NoError(t, err)

	// The mcp tool's own name can be taken too.
	_, err = m.Tools(t.Context(), []string{MetaToolName})
	require.Error(t, err)
}

// A call the server took and never answered has an unknown outcome: the
// server here writes a file and exits, so the call did take effect, and
// nothing the client received says so.
func TestCallThatGetsNoAnswerIsUnknown(t *testing.T) {
	server := stdioServer(t, "s", modeVanish)
	server.Direct = true
	m := newTestManager(t, Options{Servers: []Server{server}})
	list, err := m.Tools(t.Context(), nil)
	require.NoError(t, err)
	set := tools.Set{Tools: list}
	vanish, ok := set.Lookup("vanish")
	require.True(t, ok)

	marker := filepath.Join(t.TempDir(), "ran")
	input, err := json.Marshal(vanishInput{Path: marker})
	require.NoError(t, err)
	res := run(t, vanish, string(input))
	require.Equal(t, transcript.OutcomeUnknown, res.Outcome)
	require.FileExists(t, marker)

	// A call that could not be sent at all is a failure, not an unknown.
	missing := Server{Name: "gone", Transport: config.MCPTransportStdio, Command: filepath.Join(t.TempDir(), "missing"), Direct: false}
	m = newTestManager(t, Options{Servers: []Server{missing}})
	list, err = m.Tools(t.Context(), nil)
	require.NoError(t, err)
	res = run(t, list[0], `{"action":"call","server":"gone","tool":"echo"}`)
	require.Equal(t, transcript.OutcomeFailed, res.Outcome)
}
