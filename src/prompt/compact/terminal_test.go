package compact

import (
	"context"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/transcript"
	"github.com/stretchr/testify/require"
)

func TestControllerFinishesReadyReplay(t *testing.T) {
	path := filepath.Join("testdata", "ollama_summary.json")
	exchange, err := providertest.Load(path)
	require.NoError(t, err)
	var captured struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		System    []struct {
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(exchange.Request.Body, &captured))
	client := httpclient.New()
	t.Cleanup(client.CloseIdleConnections)
	server := providertest.Replay(t, path)
	model, err := anthropic.New(anthropic.Config{HTTPClient: client, BaseURL: server.URL, Version: "recording", Provider: "ollama"})
	require.NoError(t, err)
	req := provider.Request{
		Model: captured.Model, MaxOutputTokens: captured.MaxTokens,
		Credential: auth.Credential{Account: "local", Scheme: auth.SchemeAPIKey, Token: "compaction-test-key"},
	}
	for _, system := range captured.System {
		req.System = append(req.System, provider.SystemBlock{Name: "compaction", Text: system.Text})
	}
	for _, message := range captured.Messages {
		var parts []transcript.Part
		for _, part := range message.Content {
			parts = append(parts, transcript.Part{Text: part.Text})
		}
		req.Items = append(req.Items, transcript.Item{User: &transcript.User{Parts: parts}})
	}
	require.NotEmpty(t, req.Items)
	entries := []transcript.Entry{transcript.NewEntry(uuid.Nil(), time.Now(), req.Items[0])}
	source, err := Select(entries, 0)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pendingCtx, stop := context.WithCancel(ctx)
	task := &Task{done: make(chan struct{}), cancel: stop}
	// The model result comes only from the real recorded stream. This test
	// isolates terminal ownership, not summary request construction or quality.
	go func() {
		defer close(task.done)
		defer stop()
		result := &Result{Source: source}
		var items []transcript.Item
		var reason provider.StopReason
		for event, err := range model.Stream(pendingCtx, req) {
			if err != nil {
				task.err = err
				return
			}
			if event.Item != nil {
				items = append(items, *event.Item)
			}
			if event.Usage != nil {
				usage := *event.Usage
				result.Usage = &usage
			}
			if event.Stop != "" {
				reason = event.Stop
			}
		}
		summary, err := SummaryText(items, reason)
		if err != nil {
			task.err = err
			return
		}
		result.item = transcript.Item{Compaction: &transcript.Compaction{Summary: summary, Mode: config.CompactionReactive}}
		task.result = result
	}()
	controller := &Controller{pending: task}
	defer controller.Close()
	select {
	case <-task.done:
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	}
	require.NoError(t, task.err)
	followup := transcript.NewEntry(source.Tail, time.Now(), transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "Continue."}}}})
	entries = append(entries, followup)
	effects, err := controller.Finish(ctx, BeforeInput{Entries: entries, ProviderName: "ollama"})
	require.NoError(t, err)
	require.Len(t, effects, 2)
	require.Equal(t, task.result.Usage, effects[0].Usage)
	require.NotEmpty(t, effects[1].Compaction.Summary)
	require.Equal(t, followup.ID, effects[1].Compaction.FirstKept)
	for _, item := range effects {
		entries = append(entries, transcript.NewEntry(entries[len(entries)-1].ID, time.Now(), item))
	}
	view, err := Read(entries)
	require.NoError(t, err)
	require.Equal(t, task.result.item.Compaction.Summary, view.Summary())
	require.Equal(t, []transcript.Entry{followup, entries[len(entries)-2]}, view.Active)
	effects, err = controller.Finish(ctx, BeforeInput{Entries: entries})
	require.NoError(t, err)
	require.Empty(t, effects, "a terminal result is drained only once")
}

func TestControllerCancelsAndJoinsProvider(t *testing.T) {
	for _, finish := range []bool{false, true} {
		name := "close"
		if finish {
			name = "finish"
		}
		t.Run(name, func(t *testing.T) {
			arrived, serverCanceled := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				if _, err := io.Copy(io.Discard, req.Body); err != nil {
					return
				}
				close(arrived)
				<-req.Context().Done()
				close(serverCanceled)
				panic(http.ErrAbortHandler)
			}))
			t.Cleanup(server.Close)
			client := httpclient.New()
			t.Cleanup(client.CloseIdleConnections)
			gate := &cancelReturnGate{next: client.Transport, canceled: make(chan struct{}), release: make(chan struct{})}
			client.Transport = gate
			var once sync.Once
			release := func() { once.Do(func() { close(gate.release) }) }
			defer release()
			model, err := anthropic.New(anthropic.Config{HTTPClient: client, BaseURL: server.URL, Version: "recording", Provider: "ollama"})
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			entry := transcript.NewEntry(uuid.Nil(), time.Now(), transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "Summarize this context."}}}})
			source, err := Select([]transcript.Entry{entry}, 0)
			require.NoError(t, err)
			controller := &Controller{pending: Start(ctx, Work{
				Source: source, Mode: config.CompactionReactive, Model: model,
				// Same measured context size as the existing local recordings.
				ContextWindow: 131072,
				Input: SummaryInput{
					Prompt: config.SummaryShort, Model: "llama3.2:3b", MaxOutputTokens: 1024,
					Credential: auth.Credential{Account: "local", Scheme: auth.SchemeAPIKey, Token: "compaction-test-key"},
				},
			})}
			defer func() { release(); controller.Close() }()
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal(context.Cause(ctx))
			}
			type outcome struct {
				items  []transcript.Item
				err    error
				joined bool
			}
			closed := make(chan outcome, 1)
			go func() {
				var out outcome
				if finish {
					out.items, out.err = controller.Finish(ctx, BeforeInput{})
				} else {
					controller.Close()
				}
				out.joined = gate.returned.Load()
				closed <- out
			}()
			select {
			case <-gate.canceled:
			case <-ctx.Done():
				release()
				<-closed
				t.Fatal(context.Cause(ctx))
			}
			// Keep the transport owned until the real HTTP cancellation returns.
			release()
			out := <-closed
			require.True(t, out.joined, "controller returned before its provider goroutine exited")
			require.NoError(t, out.err)
			require.Empty(t, out.items)
			select {
			case <-serverCanceled:
			case <-ctx.Done():
				t.Fatal(context.Cause(ctx))
			}
		})
	}
}

type cancelReturnGate struct {
	next     http.RoundTripper
	canceled chan struct{}
	release  chan struct{}
	returned atomic.Bool
}

func (g *cancelReturnGate) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := g.next.RoundTrip(req)
	if err != nil && req.Context().Err() != nil {
		close(g.canceled)
		<-g.release
	}
	g.returned.Store(true)
	return resp, err
}
