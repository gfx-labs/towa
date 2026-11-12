package looptest

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/compact"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/loop"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/provider/router"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
)

// This reuses the real summary exchange. Recording makes only one new main
// request, never a new summary request or a model pull.
func TestCompactedTurn(t *testing.T) {
	scenario := Scenario{Name: "compacted_turn", Requests: 1, Model: Model, Prompt: "Reply with OK.", MaxOutputTokens: 256}
	var f *Fixture
	if os.Getenv("TOWA_RECORD_COMPACTED_TURN") != "" {
		f = build(t, scenario, OllamaBase, func(next http.RoundTripper) *sequence {
			return &sequence{recorders: []*providertest.Recorder{{Next: next, Path: RecordingPath(scenario, 1)}}}
		})
	} else {
		f = Replay(t, scenario)
	}
	old := Replay(t, ReadTurn)
	id := uuid.NewV7()
	noUpdate := func(context.Context, acp.SessionUpdate) error { return nil }
	_, err := old.Loop.Run(t.Context(), old.Turn(acp.SessionId(id.String()), noUpdate))
	require.NoError(t, err)
	before := allEntries(t, old.Store, id)
	var window int64
	for _, e := range before {
		if e.Item.Usage != nil {
			window = compact.ContextTokens(*e.Item.Usage)
		}
	}
	require.Positive(t, window)

	summaryHTTP := httpclient.New()
	summaryReplay := providertest.Replay(t, filepath.Join(filepath.Dir(RecordingPath(scenario, 1)), "../../../compact/testdata/ollama_summary.json"))
	summary, err := anthropic.New(anthropic.Config{HTTPClient: summaryHTTP, BaseURL: summaryReplay.URL, Version: "recording", Provider: ProviderID})
	require.NoError(t, err)
	source, err := auth.NewSource(auth.Options{
		Accounts: []config.Account{{ID: Account, Provider: ProviderID, Kind: config.AccountAPIKey, APIKeyEnv: "SUMMARY_TEST_KEY"}},
		Store:    auth.NewStore(t.TempDir()), HTTPClient: httpclient.New(),
		Getenv: func(string) string { return "compaction-test-key" }, TokenTimeout: time.Minute,
	})
	require.NoError(t, err)
	summaryRouter, err := router.New(router.Options{
		Auth: source, Retry: f.Config.Limits.Retry, Rand: func() float64 { return 0.5 },
		Chain: []router.Entry{{
			Provider: summary, ProviderID: ProviderID, Account: Account, Model: Model,
			Preparation: &router.Preparation{
				ContextWindow: ContextSize, MaxOutputTokens: 1024,
				System: []provider.SystemBlock{{Name: "unused", Text: "Do not replace the checkpoint prompt."}},
			},
		}},
	})
	require.NoError(t, err)
	cfg := f.Config.Compaction
	cfg.Mode, cfg.SummaryPrompt, cfg.OpenAINative = config.CompactionReactive, config.SummaryShort, config.NativeOff
	cfg.KeepRecentMessages, cfg.EmergencyKeepRecentMessages, cfg.ReactiveThresholdTokens = 0, 0, 1
	controller, err := compact.NewController(compact.ControllerOptions{
		Config: cfg, Summary: summaryRouter, SummaryContextWindow: ContextSize,
		SummaryInput: compact.SummaryInput{Model: Model, MaxOutputTokens: 2048, Credential: auth.Credential{Scheme: auth.SchemeAPIKey, Token: "compaction-test-key"}},
	})
	require.NoError(t, err)
	defer controller.Close()
	effects, err := controller.Before(t.Context(), compact.BeforeInput{Entries: before, ProviderName: ProviderID, ContextWindow: ContextSize})
	require.NoError(t, err)
	require.Empty(t, effects)
	opts := f.Options
	opts.Store = old.Store
	resolve := opts.Resolve
	opts.Resolve = func(ctx context.Context, model string) (loop.Resolved, error) {
		resolved, err := resolve(ctx, model)
		resolved.ContextSize = window
		return resolved, err
	}
	opts.NewCompactor = func(context.Context, loop.Turn, loop.Resolved) (*compact.Controller, error) { return controller, nil }
	runner, err := loop.New(opts)
	require.NoError(t, err)
	var updates []acp.SessionUpdate
	turn := f.Turn(acp.SessionId(id.String()), func(_ context.Context, u acp.SessionUpdate) error { updates = append(updates, u); return nil })
	turn.Sendable = func(acp.SessionUpdate) bool { return true }
	result, err := runner.Run(t.Context(), turn)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.NotEmpty(t, result.Final)
	require.Equal(t, 1, f.Served())
	after := allEntries(t, old.Store, id)
	require.Equal(t, before, after[:len(before)])
	var compactions, toolCalls int
	var summed transcript.Usage
	var compactionID acp.CompactionId
	newUser := after[len(before)]
	require.NotNil(t, newUser.Item.User)
	for _, e := range after[len(before):] {
		if e.Item.ToolCall != nil {
			toolCalls++
		}
		if e.Item.Usage != nil {
			summed.InputTokens += e.Item.Usage.InputTokens
			summed.OutputTokens += e.Item.Usage.OutputTokens
		}
		if e.Item.Compaction != nil {
			compactions++
			require.NotEmpty(t, e.Item.Compaction.Summary)
			require.Equal(t, newUser.ID, e.Item.Compaction.FirstKept)
			compactionID = acp.CompactionId(e.ID.String())
		}
	}
	require.Equal(t, 1, compactions)
	require.Zero(t, toolCalls, "old tool calls must not be replayed")
	require.NotNil(t, result.Usage)
	require.EqualValues(t, summed.InputTokens, result.Usage.InputTokens)
	require.EqualValues(t, summed.OutputTokens, result.Usage.OutputTokens)
	var announced bool
	for _, u := range updates {
		if u.CompactionUpdate != nil {
			require.Equal(t, compactionID, u.CompactionUpdate.CompactionId)
			announced = true
		}
	}
	require.True(t, announced)
}

func allEntries(t *testing.T, st store.Store, id uuid.UUID) []transcript.Entry {
	t.Helper()
	seq, err := st.History(t.Context(), id, store.Range{})
	require.NoError(t, err)
	var entries []transcript.Entry
	for entry, err := range seq {
		require.NoError(t, err)
		entries = append(entries, entry)
	}
	return entries
}
