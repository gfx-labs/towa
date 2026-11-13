package transcript_test

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/transcript"
)

func TestOpaqueCompatibilityRequiresKnownRequestOrigin(t *testing.T) {
	origin := transcript.Origin{Model: "model-a", Account: "account-a"}
	item := transcript.Item{
		Body: transcript.Thinking{}, Provider: "owner", Opaque: jsontext.Value(`{}`), Origin: &origin,
	}
	for _, tc := range []struct {
		name     string
		provider string
		origin   transcript.Origin
		want     bool
	}{
		{"same request identity", "owner", origin, true},
		{"different provider", "other", origin, false},
		{"different model", "owner", transcript.Origin{Model: "model-b", Account: origin.Account}, false},
		{"different account", "owner", transcript.Origin{Model: origin.Model, Account: "account-b"}, false},
		{"unknown provider", "", origin, false},
		{"unknown target model", "owner", transcript.Origin{Account: origin.Account}, false},
		{"unknown target account", "owner", transcript.Origin{Model: origin.Model}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, item.OpaqueCompatible(tc.provider, tc.origin))
		})
	}

	for _, stored := range []*transcript.Origin{nil, {}, {Model: origin.Model}, {Account: origin.Account}} {
		unknown := item
		unknown.Origin = stored
		assert.False(t, unknown.OpaqueCompatible("owner", origin))
		if stored == nil {
			require.NoError(t, unknown.Validate(), "an item without an origin is still rendered from its plain fields")
		} else {
			require.Error(t, unknown.Validate())
		}
	}
	plain := item
	plain.Opaque = nil
	assert.False(t, plain.OpaqueCompatible("owner", origin))
}

func TestItemIsStoredUnderItsBodysName(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]transcript.Body{
		"user":         transcript.User{Role: "developer", Parts: []transcript.Part{{Text: "hi"}}},
		"text":         transcript.Text{Text: "hello"},
		"thinking":     transcript.Thinking{Text: "hm"},
		"tool_call":    transcript.ToolCall{ID: "c1", Name: "read", Input: jsontext.Value(`{"path":"a"}`)},
		"tool_result":  transcript.ToolResult{CallID: "c1", Name: "read", Parts: []transcript.Part{{Text: "ok"}}, Outcome: transcript.OutcomeCompleted},
		"native":       transcript.Native{Kind: "compaction"},
		"usage":        transcript.Usage{Model: "m", InputTokens: 3},
		"model_switch": transcript.ModelSwitch{From: "a", To: "b"},
		"compaction":   transcript.Compaction{Summary: "s", Mode: "summary"},
		"interrupted":  transcript.Interrupted{},
	} {
		item := transcript.Item{
			Body: body, Provider: "owner", Opaque: jsontext.Value(`{"k":1}`),
			Origin: &transcript.Origin{Model: "m", Account: "a"},
		}
		encoded, err := json.Marshal(item)
		require.NoError(t, err, name)
		var members map[string]jsontext.Value
		require.NoError(t, json.Unmarshal(encoded, &members))
		assert.ElementsMatch(t, []string{name, "provider", "opaque", "origin"}, slices.Collect(maps.Keys(members)))
		var decoded transcript.Item
		require.NoError(t, json.Unmarshal(encoded, &decoded), name)
		assert.Equal(t, item, decoded)
	}

	var decoded transcript.Item
	require.Error(t, json.Unmarshal([]byte(`{"provider":"owner"}`), &decoded), "no body")
	require.Error(t, json.Unmarshal([]byte(`{"text":{"text":"a"},"thinking":{}}`), &decoded), "two bodies")
	_, err := json.Marshal(transcript.Item{})
	require.Error(t, err, "an item without a body is not written")
	// A body is held by value. A pointer would make every type switch miss it.
	require.Error(t, transcript.Item{Body: &transcript.Text{Text: "a"}}.Validate())
}
