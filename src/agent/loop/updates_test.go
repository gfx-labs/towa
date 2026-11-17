package loop_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/transcript"
)

// wire encodes updates as the connection does and decodes them as a client
// does, so the test sees what a client sees.
func wire(t *testing.T, updates []acp.SessionUpdate) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(updates))
	for i, u := range updates {
		raw, err := json.Marshal(u)
		require.NoError(t, err)
		var back acp.SessionUpdate
		require.NoError(t, json.Unmarshal(raw, &back), "a client can decode the update: %s", raw)
		require.NoError(t, json.Unmarshal(raw, &out[i]))
	}
	return out
}

func TestUpdatesForAToolCallAndItsResult(t *testing.T) {
	b := newBench(t)
	m := loop.Mapper{Tools: b.set()}
	path := b.path("notes.txt")
	input := jsontext.Value(`{"path":"` + path + `"}`)

	updates, err := m.Updates(transcript.Item{ToolCall: &transcript.ToolCall{ID: "c1", Name: "read", Input: input}}, &loop.Cost{})
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{
		"sessionUpdate": "tool_call",
		"toolCallId":    "c1",
		"name":          "read",
		"title":         "Read " + path,
		"kind":          "read",
		"status":        "pending",
		"locations":     []any{map[string]any{"path": path}},
		"rawInput":      map[string]any{"path": path},
	}}, wire(t, updates))

	// A tool the set does not have is still shown.
	updates, err = m.Updates(transcript.Item{ToolCall: &transcript.ToolCall{ID: "c2", Name: "gone", Input: jsontext.Value(`{}`)}}, &loop.Cost{})
	require.NoError(t, err)
	shown := wire(t, updates)[0]
	assert.Equal(t, "gone", shown["title"])
	assert.Equal(t, "other", shown["kind"])

	updates, err = m.Updates(transcript.Item{ToolResult: &transcript.ToolResult{
		CallID: "c1", Name: "read", Outcome: transcript.OutcomeUnknown,
		Parts: []transcript.Part{
			{Text: "first lines"},
			// A reference to stored content is not content for a client.
			{Stored: &transcript.Stored{Hash: "sha256:00", Size: 4096}},
		},
	}}, &loop.Cost{})
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "c1",
		"status":        "failed",
		"content": []any{map[string]any{
			"type": "content", "content": map[string]any{"type": "text", "text": "first lines"},
		}},
	}}, wire(t, updates))

	_, err = m.Updates(transcript.Item{ToolResult: &transcript.ToolResult{CallID: "c1", Outcome: "exploded"}}, &loop.Cost{})
	assert.Error(t, err, "an outcome outside the four is not shown as one of them")
}

func TestUpdatesForTextThoughtsAndUsage(t *testing.T) {
	m := loop.Mapper{ContextSize: func(model string) (int, bool) { return 200000, model == "opus" }}
	kinds := func(item transcript.Item) []any {
		updates, err := m.Updates(item, &loop.Cost{})
		require.NoError(t, err)
		var out []any
		for _, u := range wire(t, updates) {
			out = append(out, u["sessionUpdate"])
		}
		return out
	}
	assert.Equal(t, []any{"agent_message_chunk"}, kinds(transcript.Item{Text: &transcript.Text{Text: "hi"}}))
	assert.Equal(t, []any{"agent_thought_chunk"}, kinds(transcript.Item{Thinking: &transcript.Thinking{Text: "hm"}}))
	assert.Empty(t, kinds(transcript.Item{Thinking: &transcript.Thinking{}}), "reasoning only the provider can read has nothing to show")
	assert.Empty(t, kinds(transcript.Item{Interrupted: &transcript.Interrupted{}}))
	assert.Empty(t, kinds(transcript.Item{Native: &transcript.Native{Kind: "compaction"}}))

	usage := transcript.Usage{
		Model: "opus", InputTokens: 10, CacheReadTokens: 5000, CacheWriteTokens: 200, OutputTokens: 90,
		Cost: &transcript.Cost{Amount: 0.25, Currency: "USD"},
	}
	var cost loop.Cost
	updates, err := m.Updates(transcript.Item{Usage: &usage}, &cost)
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{
		"sessionUpdate": "usage_update",
		"_meta":         map[string]any{"towa": map[string]any{"model": "opus"}},
		"size":          float64(200000),
		"used":          float64(5300),
		"cost":          map[string]any{"amount": 0.25, "currency": "USD"},
	}}, wire(t, updates))

	// ACP's cost is the session's cumulative cost, not the request's.
	updates, err = m.Updates(transcript.Item{Usage: &usage}, &cost)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"amount": 0.5, "currency": "USD"}, wire(t, updates)[0]["cost"])

	// One request without a price makes the session's cost unknown from then on.
	unpriced := usage
	unpriced.Cost = nil
	for _, item := range []transcript.Usage{unpriced, usage} {
		updates, err = m.Updates(transcript.Item{Usage: &item}, &cost)
		require.NoError(t, err)
		assert.NotContains(t, wire(t, updates)[0], "cost")
	}

	usage.Model = "unknown-model"
	assert.Empty(t, kinds(transcript.Item{Usage: &usage}), "no context size is made up for a model without one")
}

func TestAPromptSurvivesTheTripThroughTheTranscript(t *testing.T) {
	mime := "text/markdown"
	prompt := []acp.ContentBlock{
		{Text: &acp.ContentBlockText{Type: "text", Text: "look at these"}},
		{Image: &acp.ContentBlockImage{Type: "image", MimeType: "image/png", Data: "iVBORw0KGgo="}},
		{Audio: &acp.ContentBlockAudio{Type: "audio", MimeType: "audio/wav", Data: "UklGRg=="}},
		{ResourceLink: &acp.ContentBlockResourceLink{Type: "resource_link", Uri: "file:///a.md", Name: "a.md", MimeType: &mime}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Uri: "file:///b.md", Text: "# b", MimeType: &mime},
		}}},
		{Resource: &acp.ContentBlockResource{Type: "resource", Resource: acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Uri: "file:///c.bin", Blob: "AAEC"},
		}}},
	}
	item, err := loop.UserItem(prompt)
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 1, 2}, item.User.Parts[5].Resource.Data)

	updates, err := loop.Mapper{}.Updates(item, &loop.Cost{})
	require.NoError(t, err)
	require.Len(t, updates, len(prompt))
	for i, u := range updates {
		assert.Equal(t, prompt[i], u.UserMessageChunk.Content, "block %d", i)
	}

	_, err = loop.UserItem([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", MimeType: "image/png", Data: "not base64!"}}})
	assert.Error(t, err)

	// A provider refuses a part that holds nothing, and a stored prompt is
	// sent again with every later request of the session.
	empty := acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: ""}}
	item, err = loop.UserItem([]acp.ContentBlock{empty, prompt[0], empty})
	require.NoError(t, err)
	assert.Equal(t, []transcript.Part{{Text: "look at these"}}, item.User.Parts)
	_, err = loop.UserItem([]acp.ContentBlock{empty})
	var refused *acp.RequestError
	require.ErrorAs(t, err, &refused)
	assert.Equal(t, acp.NewInvalidParams(nil).Code, refused.Code)
}
