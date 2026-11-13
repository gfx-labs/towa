package looptest_test

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/loop/looptest"
	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/config/configtest"
	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/sidecar/embed"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/store/dir"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/transcript"
)

// These exchanges are Ollama's emulation, not Anthropic or OpenAI proof.
// Record at most one request per test, with no retries or model pulls:
// TOWA_RECORD_COMPACTION=1 go test ./src/agent/loop/looptest -run 'TestSummaryExchange|TestSemanticExchange'
const recordEnv = "TOWA_RECORD_COMPACTION"

// compactRecording is a recording kept with the compact package, which
// replays the same summary in a test of its own.
func compactRecording(name string) string {
	return filepath.Join("..", "..", "..", "prompt", "compact", "testdata", name)
}

func recordedHistory(t *testing.T) (*looptest.Fixture, uuid.UUID, []transcript.Entry) {
	t.Helper()
	fixture := looptest.Replay(t, looptest.ReadTurn)
	id := uuid.NewV7()
	turn := fixture.Turn(acp.SessionId(id.String()), func(context.Context, acp.SessionUpdate) error { return nil })
	_, err := fixture.Loop.Run(t.Context(), turn)
	require.NoError(t, err)
	require.Equal(t, looptest.ReadTurn.Requests, fixture.Served())
	return fixture, id, history(t, fixture.Store, id)
}

func TestControllerExchange(t *testing.T) {
	if os.Getenv(recordEnv) != "" {
		t.Skip("this controller check only replays the existing summary fixture")
	}
	fixture, id, entries := recordedHistory(t)
	loaded, err := configtest.Load(config.Sources{})
	require.NoError(t, err)
	cfg := loaded.Config.Compaction
	cfg.Mode, cfg.SummaryPrompt, cfg.OpenAINative = config.CompactionReactive, config.SummaryShort, config.NativeOff
	cfg.KeepRecentMessages, cfg.EmergencyKeepRecentMessages, cfg.ReactiveThresholdTokens = 0, 0, 1
	gate := &requestGate{arrived: make(chan struct{}), release: make(chan struct{})}
	model := summaryClient(t, compactRecording("ollama_summary.json"), gate)
	opts := compact.ControllerOptions{
		Config: cfg, Summary: model, SummaryContextWindow: looptest.ContextSize,
		SummaryInput: compact.SummaryInput{
			Model: looptest.Model, MaxOutputTokens: 1024,
			Credential: auth.Credential{Account: looptest.Account, Scheme: auth.SchemeAPIKey, Token: "compaction-test-key"},
		},
	}
	controller, err := compact.NewController(opts)
	require.NoError(t, err)
	defer controller.Close()
	in := compact.BeforeInput{Entries: entries, ProviderName: looptest.ProviderID, ContextWindow: looptest.ContextSize}
	effects, err := controller.Before(t.Context(), in)
	require.NoError(t, err)
	require.Empty(t, effects)
	startup := time.NewTimer(5 * time.Second)
	defer startup.Stop()
	select {
	case <-gate.arrived:
	case <-startup.C:
		_, err := controller.Before(t.Context(), in)
		require.NoError(t, err, "summary failed before reaching the transport")
		t.Fatal("summary did not reach the transport")
	case <-t.Context().Done():
		t.Fatal(context.Cause(t.Context()))
	}
	// Lower only the decision's model budget to the observed input size,
	// forcing the emergency wait without changing the summary's request.
	for _, e := range entries {
		if transcript.Is[transcript.Usage](e.Item) {
			in.ContextWindow = compact.ContextTokens(e.Item.Body.(transcript.Usage))
		}
	}
	require.Positive(t, in.ContextWindow)
	close(gate.release)
	effects, err = controller.Before(t.Context(), in)
	require.NoError(t, err)
	require.Len(t, effects, 2)
	require.IsType(t, transcript.Usage{}, effects[0].Body)
	require.NotEmpty(t, effects[1].Body.(transcript.Compaction).Summary)
	parent := entries[len(entries)-1].ID
	for _, item := range effects {
		e := transcript.NewEntry(parent, time.Now(), item)
		require.NoError(t, fixture.Store.Append(t.Context(), id, []transcript.Entry{e}))
		parent = e.ID
	}
	all := history(t, fixture.Store, id)
	require.Equal(t, entries, all[:len(entries)])
	items, err := compact.Items(all, "portable")
	require.NoError(t, err)
	_, repairs, err := loop.Conversation(items, looptest.Interruption)
	require.NoError(t, err)
	require.Empty(t, repairs)
	require.Equal(t, looptest.ReadTurn.Requests, fixture.Served())
	controller.Close()
	in.Entries, in.ContextWindow = all, looptest.ContextSize
	effects, err = controller.Before(t.Context(), in)
	require.NoError(t, err)
	require.Empty(t, effects)
	// Enabled capabilities must not silently disappear at construction.
	opts.Config.Mode = config.CompactionSemantic
	_, err = compact.NewController(opts)
	require.Error(t, err)
	opts.Config.Mode, opts.Config.OpenAINative = config.CompactionReactive, config.NativeExplicit
	_, err = compact.NewController(opts)
	require.Error(t, err)
}

func history(t *testing.T, st store.Store, id uuid.UUID) []transcript.Entry {
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

type requestGate struct {
	next    http.RoundTripper
	arrived chan struct{}
	release chan struct{}
}

func (gate *requestGate) RoundTrip(req *http.Request) (*http.Response, error) {
	close(gate.arrived)
	select {
	case <-req.Context().Done():
		return nil, context.Cause(req.Context())
	case <-gate.release:
		return gate.next.RoundTrip(req)
	}
}

func summaryClient(t *testing.T, path string, gate *requestGate) *anthropic.Client {
	t.Helper()
	client := httpclient.New()
	base := "http://127.0.0.1:11434"
	if os.Getenv(recordEnv) != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		client.Transport = &providertest.Recorder{Next: client.Transport, Path: path, Secrets: []string{"compaction-test-key"}}
	} else {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			t.Skip("summary exchange not recorded")
		}
		base = providertest.Replay(t, path).URL
	}
	gate.next, client.Transport = client.Transport, gate
	model, err := anthropic.New(anthropic.Config{HTTPClient: client, BaseURL: base, Version: "recording", Provider: "ollama"})
	require.NoError(t, err)
	return model
}

func TestSummaryExchange(t *testing.T) {
	fixture, id, entries := recordedHistory(t)
	original := append([]transcript.Entry(nil), entries...)
	root := t.TempDir()
	st, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionNone})
	require.NoError(t, err)
	meta, _, err := fixture.Store.Load(t.Context(), id)
	require.NoError(t, err)
	require.NoError(t, st.Create(t.Context(), meta))
	require.NoError(t, st.Append(t.Context(), id, entries))
	source, err := compact.Select(entries, 0)
	require.NoError(t, err)
	gate := &requestGate{arrived: make(chan struct{}), release: make(chan struct{})}
	model := summaryClient(t, compactRecording("ollama_summary.json"), gate)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	task := compact.Start(ctx, compact.Work{
		Source: source, Mode: config.CompactionReactive, Model: model, ContextWindow: looptest.ContextSize,
		Input: compact.SummaryInput{
			Prompt: config.SummaryShort, Model: looptest.Model, MaxOutputTokens: 1024,
			Credential: auth.Credential{Account: looptest.Account, Scheme: auth.SchemeAPIKey, Token: "compaction-test-key"},
		},
	})
	defer task.Cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := task.Wait(ctx, time.Minute)
		finished <- err
	}()
	// Append while the model is working. A nil captured FirstKept must not
	// cause this new entry to disappear from the next context.
	followup := transcript.NewEntry(entries[len(entries)-1].ID, time.Now(), transcript.Item{
		Body: transcript.User{Parts: []transcript.Part{{Text: "Continue from this checkpoint."}}},
	})
	require.NoError(t, st.Append(t.Context(), id, []transcript.Entry{followup}))
	entries = history(t, st, id)
	select {
	case <-gate.arrived:
	case err := <-finished:
		t.Fatalf("summary finished before reaching the transport: %v", err)
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	_, err = task.Wait(ctx, 0)
	require.ErrorIs(t, err, compact.ErrPending)
	waitCtx, endWait := context.WithCancel(ctx)
	endWait()
	_, err = task.Wait(waitCtx, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	waitCtx, endWait = context.WithTimeout(ctx, time.Millisecond)
	defer endWait()
	_, err = task.Wait(waitCtx, time.Minute)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	close(gate.release)
	got, err := task.Wait(ctx, time.Minute)
	require.NoError(t, err)
	_, err = providertest.Load(compactRecording("ollama_summary.json"))
	require.NoError(t, err)
	item, err := got.ItemFor(entries)
	require.NoError(t, err)
	require.Equal(t, followup.ID, item.Body.(transcript.Compaction).FirstKept)
	require.NotEmpty(t, item.Body.(transcript.Compaction).Summary)
	require.NotNil(t, got.Usage)
	entry := transcript.NewEntry(followup.ID, time.Now(), item)
	require.NoError(t, st.Append(t.Context(), id, []transcript.Entry{entry}))
	// Open another store instance, as a restarted process does.
	reopened, err := dir.New(dir.Options{Root: root, Compression: dir.CompressionNone})
	require.NoError(t, err)
	all := history(t, reopened, id)
	require.Equal(t, original, all[:len(original)])
	require.Len(t, all, len(original)+2)
	items, err := compact.Items(all, "another-provider")
	require.NoError(t, err)
	items, repairs, err := loop.Conversation(items, looptest.Interruption)
	require.NoError(t, err)
	require.Empty(t, repairs)
	require.Len(t, items, 2)
	require.Contains(t, items[0].Body.(transcript.User).Parts[0].Text, item.Body.(transcript.Compaction).Summary)
	require.Equal(t, followup.Item, items[1])
	require.Equal(t, looptest.ReadTurn.Requests, fixture.Served())
	_, err = got.ItemFor(all)
	require.ErrorIs(t, err, compact.ErrSuperseded)
}

func TestSemanticExchange(t *testing.T) {
	_, _, entries := recordedHistory(t)
	path := compactRecording("ollama_semantic.json")
	client := httpclient.New()
	base := "http://127.0.0.1:11434/v1"
	if os.Getenv(recordEnv) != "" {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		client.Transport = &providertest.Recorder{Next: client.Transport, Path: path}
	} else {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			t.Skip("semantic exchange not recorded")
		}
		base = providertest.Replay(t, path).URL + "/v1"
	}
	cfg := config.Compaction{KeepRecentMessages: 1, GoalWindowMessages: 1, EmbedMaxChars: 512, RelevanceKeepThreshold: 0.65}
	embedder, err := embed.New(embed.Options{HTTPClient: client, Config: config.Embeddings{
		BaseURL: base, Model: looptest.Model, BatchSize: 128, TimeoutSeconds: 60,
	}})
	require.NoError(t, err)
	keep, gotErr := compact.SemanticKeep(t.Context(), embedder, cfg, entries)
	exchange, err := providertest.Load(path)
	require.NoError(t, err)
	if exchange.Response.Status != 200 {
		var refusal *embed.Error
		require.ErrorAs(t, gotErr, &refusal)
		require.Equal(t, exchange.Response.Status, refusal.Status)
		require.Zero(t, keep)
		t.Logf("loaded model cannot serve this embedding request: HTTP %d; semantic decisions remain unvalidated", refusal.Status)
		return
	}
	require.NoError(t, gotErr)
	require.GreaterOrEqual(t, keep, cfg.KeepRecentMessages)
	require.LessOrEqual(t, keep, compact.Messages(entries))
}
