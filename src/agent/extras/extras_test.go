package extras_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/extras"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/prompt/agentctx"
	"github.com/gfx-labs/towa/src/sidecar/jev"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/core"
	"github.com/gfx-labs/towa/src/tools/repair"
	"github.com/gfx-labs/towa/src/transcript"
)

func run(t *testing.T, tool tools.Tool, input string, env tools.Env) tools.Result {
	t.Helper()
	schema, err := repair.Compile(tool.Spec.InputSchema)
	require.NoError(t, err)
	fixed, _, err := schema.Apply(jsontext.Value(input), false)
	require.NoError(t, err)
	desc, err := tool.Describe(fixed)
	require.NoError(t, err)
	require.NotEmpty(t, desc.Title)
	res, err := tool.Run(t.Context(), tools.Call{Input: fixed, Env: env})
	require.NoError(t, err)
	require.NotEmpty(t, res.Outcome)
	return res
}

func text(res tools.Result) string {
	var out strings.Builder
	for _, p := range res.Parts {
		out.WriteString(p.Text)
	}
	return out.String()
}

func searchConfig() config.ConversationSearch {
	return config.ConversationSearch{Enabled: true, Scope: "descendants", MaxHits: 10, SnippetContextBytes: 50, MessageBytes: 1000, ToolResultBytes: 200}
}

func TestConversationSearchRunsOnFullStoredHistory(t *testing.T) {
	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionNone})
	require.NoError(t, err)
	parent, child, other := uuid.NewV7(), uuid.NewV7(), uuid.NewV7()
	now := time.Now()
	for _, m := range []store.Meta{{ID: parent}, {ID: child, ParentID: parent}, {ID: other}} {
		m.Cwd = t.TempDir()
		m.Created = now
		m.Updated = now
		require.NoError(t, st.Create(t.Context(), m))
		entries := []transcript.Entry{
			transcript.NewEntry(uuid.UUID{}, now, transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "the old quokka marker"}}}}),
		}
		kept := transcript.NewEntry(entries[0].ID, now.Add(time.Second), transcript.Item{Text: &transcript.Text{Text: "recent text without the marker"}})
		entries = append(entries, kept, transcript.NewEntry(kept.ID, now.Add(2*time.Second), transcript.Item{Compaction: &transcript.Compaction{Summary: "A prior exchange", FirstKept: kept.ID, Mode: "summary"}}))
		require.NoError(t, st.Append(t.Context(), m.ID, entries))
	}
	tool, err := extras.ConversationSearch(searchConfig(), st)
	require.NoError(t, err)
	env := tools.Env{SessionID: acp.SessionId(parent.String())}
	res := run(t, tool, `{"query":"QUOKKA","stats":true,"turns":{"start":0,"end":2}}`, env)
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Contains(t, text(res), "Found 2 matches")
	require.Contains(t, text(res), child.String())
	require.NotContains(t, text(res), other.String())
	require.Contains(t, text(res), "the old quokka marker")
	require.Equal(t, text(res), res.Content[0].Content.Content.Text.Text)
	for _, input := range []string{`{"scope":"store","query":"quokka"}`, `{"session":"` + other.String() + `","query":"quokka"}`, `{"query":""}`, `{"turns":{"start":2,"end":1}}`} {
		require.Equal(t, transcript.OutcomeFailed, run(t, tool, input, env).Outcome)
	}
}

func TestDisabledAndMissingDependenciesAreNotUsableTools(t *testing.T) {
	_, err := extras.ConversationSearch(config.ConversationSearch{}, nil)
	require.ErrorIs(t, err, extras.ErrDisabled)
	_, err = extras.WebFetch(config.WebFetch{}, nil)
	require.ErrorIs(t, err, extras.ErrDisabled)
	_, err = extras.Batch(config.ToolsBatch{}, nil)
	require.ErrorIs(t, err, extras.ErrDisabled)
	_, err = extras.Skill(config.Context{}, nil)
	require.ErrorIs(t, err, extras.ErrDisabled)
	_, err = extras.JevDecision(nil, nil)
	require.ErrorIs(t, err, extras.ErrDisabled)
	_, err = extras.ConversationSearch(searchConfig(), nil)
	require.Error(t, err)
	_, err = extras.WebFetch(fetchConfig(), nil)
	require.Error(t, err)
	_, err = extras.Batch(config.ToolsBatch{Enabled: true, MaxCalls: 10}, nil)
	require.Error(t, err)
}

func fetchConfig() config.WebFetch {
	return config.WebFetch{Enabled: true, Timeout: time.Second, MaxBodyBytes: 1 << 20, MaxResultBytes: 1 << 20, UserAgent: "towa-test"}
}

func TestWebFetchUsesRealHTTPAndReportsCapabilityErrors(t *testing.T) {
	srv := httptest.NewServer(http.FileServer(http.Dir("../../tools/webfetch/testdata")))
	defer srv.Close()
	tool, err := extras.WebFetch(fetchConfig(), httpclient.New())
	require.NoError(t, err)
	// These are the capability's recorded public pages, not invented HTML.
	entries, err := os.ReadDir("../../tools/webfetch/testdata")
	require.NoError(t, err)
	var path string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".html") {
			path = e.Name()
			break
		}
	}
	require.NotEmpty(t, path)
	input, err := json.Marshal(map[string]string{"url": srv.URL + "/" + path})
	require.NoError(t, err)
	res := run(t, tool, string(input), tools.Env{})
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Contains(t, text(res), "Fetched "+srv.URL)
	require.Greater(t, len(text(res)), len(srv.URL)+50)
	for _, url := range []string{srv.URL + "/not-a-page", "file:///etc/passwd", "://"} {
		input, err = json.Marshal(map[string]string{"url": url})
		require.NoError(t, err)
		require.Equal(t, transcript.OutcomeFailed, run(t, tool, string(input), tools.Env{}).Outcome)
	}
}

func skillTool(t *testing.T) tools.Tool {
	t.Helper()
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "audit")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: audit\ndescription: Inspect a change\n---\nRead the diff and check errors.\n"), 0o600))
	skills, err := agentctx.ListSkills(agentctx.Request{WorkDir: root, Config: config.Context{SkillsDirs: []string{filepath.Join(root, "skills")}}})
	require.NoError(t, err)
	tool, err := extras.Skill(config.Context{SkillsEnabled: true}, skills)
	require.NoError(t, err)
	return tool
}

func TestSkillRunLoadsListedFile(t *testing.T) {
	tool := skillTool(t)
	res := run(t, tool, `{"name":" /audit","action":"load"}`, tools.Env{})
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Contains(t, text(res), "Read the diff and check errors.")
	require.Contains(t, text(res), "**Base directory**:")
	require.Equal(t, transcript.OutcomeFailed, run(t, tool, `{"name":"absent"}`, tools.Env{}).Outcome)
	require.Equal(t, transcript.OutcomeFailed, run(t, tool, `{"name":""}`, tools.Env{}).Outcome)
}

func TestJevToolSendsRequestButDoesNotInventAnAnswer(t *testing.T) {
	sent := make(chan jsontext.Value, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		sent <- body
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client, err := jev.New(jev.Options{HTTPClient: httpclient.New(), Config: config.Jev{Route: config.JevCustom, BaseURL: srv.URL, Model: "test", Timeout: time.Second, MaxTimeout: time.Second, MaxRequestBytes: 80 << 10, MaxResponseBytes: 80 << 10, MaxTextChars: 4000}})
	require.NoError(t, err)
	tool, err := extras.JevDecision(client, func(context.Context) (auth.Credential, error) { return auth.Credential{}, nil })
	require.NoError(t, err)
	res := run(t, tool, `{"instructions":"Pick the lower cost","state":{"cost":2},"options":{"a":"one","b":"two"},"timeout_ms":500}`, tools.Env{})
	require.Equal(t, transcript.OutcomeFailed, res.Outcome)
	require.Contains(t, text(res), "HTTP 503")
	var req struct {
		Questions map[string]struct {
			Instructions string            `json:"instructions"`
			Criteria     map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	require.NoError(t, json.Unmarshal(<-sent, &req))
	require.Contains(t, req.Questions["decision"].Instructions, "Pick the lower cost")
	require.Contains(t, req.Questions["decision"].Instructions, "untrusted data")
	require.Equal(t, map[string]string{"a": "one", "b": "two"}, req.Questions["decision"].Criteria)
}

func TestBatchRunsRealToolsThroughLoopAndRejectsNesting(t *testing.T) {
	skill := skillTool(t)
	bt, err := extras.Batch(config.ToolsBatch{Enabled: true, MaxCalls: 10}, loop.BatchDispatcher)
	require.NoError(t, err)
	set := tools.Set{Tools: []tools.Tool{skill, bt}}
	schemas, err := loop.Schemas(set)
	require.NoError(t, err)
	var shown strings.Builder
	d := loop.Dispatcher{Tools: set, Schemas: schemas, Logger: slog.New(slog.DiscardHandler), Update: func(update acp.SessionToolCallUpdate) {
		for _, content := range update.Content {
			if content.Content != nil && content.Content.Content.Text != nil {
				shown.WriteString(content.Content.Content.Text.Text)
			}
		}
	}}
	res := d.Run(t.Context(), []transcript.ToolCall{{ID: "batch1", Name: bt.Spec.Name, Input: jsontext.Value(`{"tool_calls":[{"tool":"skill_manage","intent":"Read skill","name":"audit"},{"tool":"skill_manage","intent":"Missing skill","name":"absent"}]}`)}})
	require.NoError(t, res.Err)
	require.EqualValues(t, 3, res.Ran)
	require.Len(t, res.Results, 1)
	require.Equal(t, transcript.OutcomeFailed, res.Results[0].Outcome)
	output := text(tools.Result{Parts: res.Results[0].Parts})
	require.Contains(t, output, "Read the diff and check errors.")
	require.Contains(t, output, "not found")
	require.Contains(t, output, "1 succeeded, 1 failed")
	require.Contains(t, shown.String(), "Read the diff and check errors.")
	require.Contains(t, shown.String(), "1 succeeded, 1 failed")
	result := run(t, bt, `{"tool_calls":[{"tool":"batch","intent":"Nested","tool_calls":[]}]}`, tools.Env{})
	require.Equal(t, transcript.OutcomeFailed, result.Outcome)
}

// The file tools take no member but their own, so an entry that had to carry
// one more was refused by each of them.
func TestBatchEntryIsTheToolAndItsOwnArguments(t *testing.T) {
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	env := tools.Env{Cwd: t.TempDir()}
	require.NoError(t, os.WriteFile(filepath.Join(env.Cwd, "a.txt"), []byte("alpha\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(env.Cwd, "b.txt"), []byte("beta\n"), 0o600))
	var tracker core.ReadTracker
	fileTools, err := core.Tools(env, loaded.Config.Tools, core.Deps{Reads: &tracker})
	require.NoError(t, err)
	bt, err := extras.Batch(config.ToolsBatch{Enabled: true, MaxCalls: 10}, loop.BatchDispatcher)
	require.NoError(t, err)
	set := tools.Set{Tools: append(fileTools, bt)}
	schemas, err := loop.Schemas(set)
	require.NoError(t, err)
	d := loop.Dispatcher{Tools: set, Schemas: schemas, Env: env, Logger: slog.New(slog.DiscardHandler), Update: func(acp.SessionToolCallUpdate) {}}

	res := d.Run(t.Context(), []transcript.ToolCall{{ID: "batch1", Name: bt.Spec.Name, Input: jsontext.Value(`{"tool_calls":[{"tool":"read","file_path":"a.txt"},{"tool":"read","file_path":"b.txt"}]}`)}})
	require.NoError(t, res.Err)
	require.Len(t, res.Results, 1)
	output := text(tools.Result{Parts: res.Results[0].Parts})
	require.Equal(t, transcript.OutcomeCompleted, res.Results[0].Outcome, output)
	require.Contains(t, output, "alpha")
	require.Contains(t, output, "beta")
	require.Contains(t, output, "2 succeeded, 0 failed")
}
