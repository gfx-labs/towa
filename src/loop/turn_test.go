package loop_test

import (
	"context"
	"encoding/json/v2"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/hooks"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/loop/looptest"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/store"
	"github.com/gfx-labs/acpharness/src/store/dir"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// client collects updates after the trip through JSON, as an ACP client
// sees them. It can end the turn when an update arrives.
type client struct {
	mu      sync.Mutex
	updates []acp.SessionUpdate
	when    func(acp.SessionUpdate)
}

func (c *client) update(_ context.Context, update acp.SessionUpdate) error {
	raw, err := json.Marshal(update)
	if err != nil {
		return err
	}
	var back acp.SessionUpdate
	if err := json.Unmarshal(raw, &back); err != nil {
		return err
	}
	c.mu.Lock()
	c.updates = append(c.updates, back)
	c.mu.Unlock()
	if c.when != nil {
		c.when(back)
	}
	return nil
}

func entries(t *testing.T, st store.Store, id acp.SessionId) (store.Meta, []transcript.Entry) {
	t.Helper()
	uid, err := uuid.Parse(string(id))
	require.NoError(t, err)
	meta, seq, err := st.Load(t.Context(), uid)
	require.NoError(t, err)
	var got []transcript.Entry
	for entry, err := range seq {
		require.NoError(t, err)
		got = append(got, entry)
	}
	return meta, got
}

func TestPromptDenyNeverResolvesOrStoresTheTurn(t *testing.T) {
	st, err := dir.New(dir.Options{Root: t.TempDir(), Compression: dir.CompressionNone})
	require.NoError(t, err)
	l, err := loop.New(loop.Options{
		Resolve: func(context.Context, string) (loop.Resolved, error) {
			t.Error("denied prompt reached model resolution")
			return loop.Resolved{}, nil
		},
		Store: st, System: func(context.Context, loop.Turn) ([]provider.SystemBlock, error) { return nil, nil },
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
		Hooks: hooks.New([]config.Hook{{Event: config.HookPrompt, Command: []string{"/bin/sh", "-c", `cat >/dev/null; printf '%s' '{"decision":"deny","reason":"not allowed"}'`}, Timeout: time.Second}}),
	})
	require.NoError(t, err)
	id := uuid.NewV7()
	var c client
	_, err = l.Run(t.Context(), loop.Turn{SessionID: acp.SessionId(id.String()), Cwd: t.TempDir(), Update: c.update, Prompt: []acp.ContentBlock{{Text: &acp.ContentBlockText{Type: "text", Text: "hello"}}}})
	var stopped *loop.StopError
	require.ErrorAs(t, err, &stopped)
	assert.Equal(t, harness.StopDenied, stopped.Stop.Kind)
	assert.Equal(t, "not allowed", stopped.Stop.Message)
	_, _, err = st.Load(t.Context(), id)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Empty(t, c.updates)
}

func TestAStopHookCannotKeepATurnGoingForever(t *testing.T) {
	f := looptest.Replay(t, looptest.ReadTurn)
	opts := f.Options
	opts.Limits.MaxStopDenials = 0
	opts.Hooks = hooks.New([]config.Hook{{Event: config.HookStop, Command: []string{"/bin/sh", "-c", `cat >/dev/null; printf '%s' '{"decision":"deny","reason":"continue"}'`}, Timeout: time.Second}})
	l, err := loop.New(opts)
	require.NoError(t, err)
	var c client
	result, err := l.Run(t.Context(), f.Turn(acp.SessionId(uuid.NewV7().String()), c.update))
	require.NoError(t, err)
	assert.Equal(t, acp.StopReasonMaxTurnRequests, result.StopReason)
	require.NotNil(t, result.Stop)
	assert.Equal(t, config.KeyMaxStopDenials, result.Stop.Limit)
	assert.Contains(t, result.Final, "7421")
	assert.Equal(t, 2, f.Served())
}

func TestARecordedTurnThroughTheClientRouterLoopAndStore(t *testing.T) {
	f := looptest.Replay(t, looptest.ReadTurn)
	var c client
	id := acp.SessionId(uuid.NewV7().String())
	parent := acp.SessionId(uuid.NewV7().String())
	turn := f.Turn(id, c.update)
	turn.ParentID = parent

	// The loop resolves once, with the session's name as it is, not a
	// provider's own model name. The resolver owns the default.
	var names []string
	opts := f.Options
	resolve := opts.Resolve
	opts.Resolve = func(ctx context.Context, model string) (loop.Resolved, error) {
		names = append(names, model)
		return resolve(ctx, model)
	}
	l, err := loop.New(opts)
	require.NoError(t, err)
	result, err := l.Run(t.Context(), turn)
	require.NoError(t, err)
	assert.Equal(t, []string{""}, names)
	assert.Equal(t, 2, f.Served())
	assert.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	assert.Nil(t, result.Stop)
	require.NotNil(t, result.Usage)
	assert.Contains(t, result.Final, "7421")

	meta, got := entries(t, f.Store, id)
	assert.Equal(t, uuid.MustParse(string(parent)), meta.ParentID)
	assert.Equal(t, f.Cwd, meta.Cwd)
	// The user, the call, usage of request 1, the result, the text and
	// usage of request 2. The call was stored before its result.
	var kinds []string
	var call *transcript.ToolCall
	var answer *transcript.ToolResult
	var text string
	var usage loop.Spent
	for i, entry := range got {
		assert.NotEqual(t, uuid.Nil(), entry.ID)
		if i > 0 {
			assert.Equal(t, got[i-1].ID, entry.ParentID)
		}
		switch item := entry.Item; {
		case item.User != nil:
			kinds = append(kinds, "user")
			assert.Equal(t, looptest.ReadTurn.Prompt, item.User.Parts[0].Text)
		case item.ToolCall != nil:
			kinds = append(kinds, "call")
			call = item.ToolCall
			assert.Equal(t, looptest.ReadToolName, call.Name)
			assert.JSONEq(t, `{"path":"notes.txt"}`, string(call.Input))
		case item.ToolResult != nil:
			kinds = append(kinds, "result")
			answer = item.ToolResult
			assert.Equal(t, transcript.OutcomeCompleted, answer.Outcome)
			assert.Equal(t, "The launch code is 7421.\n", answer.Parts[0].Text)
		case item.Text != nil:
			kinds = append(kinds, "text")
			text += item.Text.Text
		case item.Usage != nil:
			kinds = append(kinds, "usage")
			assert.Equal(t, looptest.Model, item.Usage.Model)
			assert.Equal(t, looptest.Account, item.Usage.Account)
			usage.Add(*item.Usage)
		default:
			t.Fatalf("unexpected item: %+v", item)
		}
	}
	assert.Equal(t, []string{"user", "call", "usage", "result", "text", "usage"}, kinds)
	require.NotNil(t, call)
	require.NotNil(t, answer)
	assert.Equal(t, call.ID, answer.CallID)
	assert.Equal(t, text, result.Final)
	assert.Equal(t, usage.Usage(), result.Usage, "the response totals are the sum of the requests the store holds")
	assert.Positive(t, result.Usage.InputTokens)
	assert.Positive(t, result.Usage.OutputTokens)

	var shown strings.Builder
	var callUpdate *acp.SessionUpdateToolCall
	var lastTool *acp.SessionToolCallUpdate
	var usages []acp.SessionUsageUpdate
	for _, u := range c.updates {
		switch {
		case u.AgentMessageChunk != nil:
			shown.WriteString(u.AgentMessageChunk.Content.Text.Text)
		case u.ToolCall != nil:
			callUpdate = u.ToolCall
		case u.ToolCallUpdate != nil:
			lastTool = u.ToolCallUpdate
		case u.UsageUpdate != nil:
			usages = append(usages, *u.UsageUpdate)
		default:
			t.Fatalf("unexpected update: %+v", u)
		}
	}
	assert.Equal(t, text, shown.String(), "the client saw each text piece once")
	require.NotNil(t, callUpdate)
	assert.Equal(t, acp.ToolCallId(call.ID), callUpdate.ToolCallId)
	assert.Equal(t, "Read notes.txt", callUpdate.Title)
	assert.Equal(t, acp.ToolKindRead, callUpdate.Kind)
	assert.Equal(t, []acp.ToolCallLocation{{Path: "notes.txt"}}, callUpdate.Locations)
	require.NotNil(t, lastTool)
	assert.Equal(t, acp.ToolCallStatusCompleted, *lastTool.Status)
	require.Len(t, lastTool.Content, 1)
	assert.Equal(t, answer.Parts[0].Text, lastTool.Content[0].Content.Content.Text.Text)
	require.Len(t, usages, 2)
	for _, u := range usages {
		assert.EqualValues(t, looptest.ContextSize, u.Size)
		assert.Positive(t, u.Used)
	}
}

func TestACancelMidStreamStoresAnInterruptionAndNotTheUnfinishedText(t *testing.T) {
	// Cancel when the second request starts showing text, after the tool
	// call and its result have been stored.
	f := looptest.Replay(t, looptest.ReadTurn)
	id := acp.SessionId(uuid.NewV7().String())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var c client
	c.when = func(u acp.SessionUpdate) {
		if u.AgentMessageChunk != nil {
			cancel()
		}
	}
	result, err := f.Loop.Run(ctx, f.Turn(id, c.update))
	require.NoError(t, err)
	assert.Equal(t, acp.StopReasonCancelled, result.StopReason)
	assert.Empty(t, result.Final, "no text item finished")
	_, got := entries(t, f.Store, id)
	require.NotEmpty(t, got)
	assert.NotNil(t, got[len(got)-1].Item.Interrupted)
	for _, entry := range got {
		assert.Nil(t, entry.Item.Text, "unfinished text is not a finished item")
	}
	assert.Equal(t, 2, f.Served())
	// The first request's usage was reported before the cancel; the
	// unfinished second request's was not.
	require.NotNil(t, result.Usage)
	var spent loop.Spent
	for _, entry := range got {
		if entry.Item.Usage != nil {
			spent.Add(*entry.Item.Usage)
		}
	}
	assert.Equal(t, spent.Usage(), result.Usage)
}

func TestAnUnknownModel404IsATypedStop(t *testing.T) {
	f := looptest.Replay(t, looptest.UnknownModel)
	var c client
	id := acp.SessionId(uuid.NewV7().String())
	_, err := f.Loop.Run(t.Context(), f.Turn(id, c.update))
	var stopped *loop.StopError
	require.ErrorAs(t, err, &stopped)
	assert.Equal(t, harness.StopRequest, stopped.Stop.Kind)
	assert.Equal(t, looptest.Account, stopped.Stop.Account)
	assert.Equal(t, looptest.UnknownModel.Model, stopped.Stop.Model)
	assert.Contains(t, stopped.Stop.Message, "not found")
	assert.Empty(t, c.updates)
	_, got := entries(t, f.Store, id)
	require.Len(t, got, 1)
	assert.NotNil(t, got[0].Item.User)
}

func TestADeadLoginIsAnAuthStop(t *testing.T) {
	refused := &auth.LoginRequiredError{
		Account: "work", Provider: "claude", RejectedAt: time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), Code: "invalid_grant",
	}
	err := loop.StopFor(refused)
	var stopped *loop.StopError
	require.ErrorAs(t, err, &stopped)
	assert.Equal(t, harness.StopAuth, stopped.Stop.Kind)
	assert.Equal(t, "work", stopped.Stop.Account)
	assert.Contains(t, stopped.Stop.Message, "log in again")
	assert.ErrorIs(t, err, refused)
}
