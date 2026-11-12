package loop

import (
	"encoding/json/jsontext"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func TestPreparationUsesSelectedEntry(t *testing.T) {
	tn := &turn{
		l: &Loop{o: Options{Now: time.Now}}, started: time.Now(),
		model:   Resolved{ContextSize: 4096, Interruption: "first prompt"},
		entries: []transcript.Entry{{Item: transcript.Item{Interrupted: &transcript.Interrupted{}}}},
	}
	selected := &router.Preparation{
		ContextWindow: 2048, MaxOutputTokens: 128, Interruption: "selected prompt",
		System: []provider.SystemBlock{{Name: "selected", Text: "selected system"}},
	}
	req, err := tn.prepareRequest(t.Context(), router.Entry{Preparation: selected}, provider.Request{MaxOutputTokens: 512})
	require.NoError(t, err)
	require.Equal(t, selected.System, req.System)
	require.Equal(t, selected.MaxOutputTokens, req.MaxOutputTokens)
	require.Len(t, req.Items, 1)
	require.Equal(t, selected.Interruption, req.Items[0].User.Parts[0].Text)
	size, ok := tn.mapper.ContextSize("selected")
	require.True(t, ok)
	require.EqualValues(t, selected.ContextWindow, size)
	require.NotNil(t, tn.entries[0].Item.Interrupted, "rendering must not rewrite history")
	selected.ContextWindow = 0
	_, err = tn.prepareRequest(t.Context(), router.Entry{Preparation: selected}, provider.Request{})
	require.Error(t, err, "an invalid entry must not reuse the first model's window")
}

// An entry whose provider runs web search itself gets that tool in place of
// the session's own, and an entry without it keeps the session's.
func TestPreparationSwapsTheSearchToolPerEntry(t *testing.T) {
	read := provider.Tool{Name: "read", InputSchema: jsontext.Value(`{"type":"object"}`)}
	local := provider.Tool{Name: "websearch", InputSchema: jsontext.Value(`{"type":"object"}`)}
	hosted := provider.Tool{Name: "web_search", Native: jsontext.Value(`{"type":"web_search"}`), NativeAPI: provider.NativeAPIResponses}
	prepare := func(session []provider.Tool, entry router.Preparation) []provider.Tool {
		var set tools.Set
		for _, spec := range session {
			set.Tools = append(set.Tools, tools.Tool{Spec: spec})
		}
		tn := &turn{l: &Loop{o: Options{Now: time.Now}}, started: time.Now(), model: Resolved{Interruption: "i", Tools: set}}
		entry.ContextWindow, entry.MaxOutputTokens = 2048, 128
		// The request arrives holding what the entry before it was sent.
		req, err := tn.prepareRequest(t.Context(), router.Entry{Preparation: &entry}, provider.Request{Tools: []provider.Tool{hosted}})
		require.NoError(t, err)
		return req.Tools
	}
	with := router.Preparation{HostedSearch: hosted, LocalSearch: "websearch"}
	require.Equal(t, []provider.Tool{read, hosted}, prepare([]provider.Tool{read, local}, with))
	require.Equal(t, []provider.Tool{read, local}, prepare([]provider.Tool{read, local}, router.Preparation{}))
	// A session with search turned off does not get the provider's.
	require.Equal(t, []provider.Tool{read}, prepare([]provider.Tool{read}, with))
}
