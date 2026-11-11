package loop

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func TestPreparationUsesSelectedEntry(t *testing.T) {
	tn := &turn{
		l: &Loop{o: Options{Now: time.Now}}, started: time.Now(),
		model:   Resolved{ContextSize: 4096, Interruption: "first prompt"},
		entries: []transcript.Entry{{Item: transcript.Item{Interrupted: &transcript.Interrupted{}}}},
	}
	selected := &router.Preparation{
		ContextWindow: 2048, MaxOutputTokens: 128, Interruption: "selected prompt", InterruptionRole: "developer",
		System: []provider.SystemBlock{{Name: "selected", Text: "selected system"}},
	}
	req, err := tn.prepareRequest(t.Context(), router.Entry{Preparation: selected}, provider.Request{MaxOutputTokens: 512})
	require.NoError(t, err)
	require.Equal(t, selected.System, req.System)
	require.Equal(t, selected.MaxOutputTokens, req.MaxOutputTokens)
	require.Len(t, req.Items, 1)
	require.Equal(t, "developer", req.Items[0].User.Role)
	require.Equal(t, selected.Interruption, req.Items[0].User.Parts[0].Text)
	size, ok := tn.mapper.ContextSize("selected")
	require.True(t, ok)
	require.EqualValues(t, selected.ContextWindow, size)
	require.NotNil(t, tn.entries[0].Item.Interrupted, "rendering must not rewrite history")
	selected.ContextWindow = 0
	_, err = tn.prepareRequest(t.Context(), router.Entry{Preparation: selected}, provider.Request{})
	require.Error(t, err, "an invalid entry must not reuse the first model's window")
	selected.ContextWindow, selected.InterruptionRole = 2048, "unsupported"
	_, err = tn.prepareRequest(t.Context(), router.Entry{Preparation: selected}, provider.Request{})
	require.Error(t, err, "an unsupported interruption role must not be downgraded")
}
