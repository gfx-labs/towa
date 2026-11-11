package loop_test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/pkg/acp/harness"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/models"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func TestExceededNamesTheLimitThatIsUsedUp(t *testing.T) {
	limits := config.Limits{MaxWall: time.Minute, MaxInputTokens: 1000, MaxOutputTokens: 100}
	var spent loop.Spent
	spent.Add(transcript.Usage{InputTokens: 100, CacheReadTokens: 800, OutputTokens: 40})
	_, over := loop.Exceeded(limits, spent)
	assert.False(t, over)

	// Cached tokens count as sent: 100+800 and 50+49+1.
	spent.Add(transcript.Usage{InputTokens: 50, CacheReadTokens: 49, CacheWriteTokens: 1, OutputTokens: 40})
	key, over := loop.Exceeded(limits, spent)
	assert.True(t, over)
	assert.Equal(t, config.KeyMaxInputTokens, key)

	limits.MaxInputTokens = 0
	_, over = loop.Exceeded(limits, spent)
	assert.False(t, over, "zero turns a limit off")

	spent.Add(transcript.Usage{OutputTokens: 20})
	key, _ = loop.Exceeded(limits, spent)
	assert.Equal(t, config.KeyMaxOutputTokens, key)

	spent.Wall = time.Minute
	key, _ = loop.Exceeded(limits, spent)
	assert.Equal(t, config.KeyMaxWall, key)

	result := loop.LimitResult(key, spent)
	assert.Equal(t, acp.StopReasonMaxTurnRequests, result.StopReason)
	assert.Equal(t, &harness.Stop{Kind: harness.StopLimit, Limit: config.KeyMaxWall}, result.Stop)
	// The totals: input excludes the cache counts, the total includes them.
	require.NotNil(t, result.Usage)
	assert.Equal(t, 150, result.Usage.InputTokens)
	assert.Equal(t, 100, result.Usage.OutputTokens)
	assert.Equal(t, 849, *result.Usage.CachedReadTokens)
	assert.Equal(t, 1, *result.Usage.CachedWriteTokens)
	assert.Nil(t, result.Usage.ThoughtTokens)
	assert.Equal(t, 1100, result.Usage.TotalTokens)

	assert.Nil(t, loop.Spent{}.Usage(), "a turn that made no request reports no usage")
}

func TestToolCallsLeft(t *testing.T) {
	left, limited := loop.ToolCallsLeft(config.Limits{MaxToolCalls: 5}, loop.Spent{ToolCalls: 3})
	assert.True(t, limited)
	assert.EqualValues(t, 2, left)
	_, limited = loop.ToolCallsLeft(config.Limits{}, loop.Spent{ToolCalls: 3})
	assert.False(t, limited)
}

func TestStopForCopiesWhatTheProviderSaid(t *testing.T) {
	reset := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	entry := router.Entry{ProviderID: "claude", Account: "work", Model: "opus"}
	perr := &provider.Error{
		Class: provider.Fallback, Reason: provider.ReasonRateLimit, Status: 429,
		RetryAfter: 90 * time.Second, ResetAt: reset, Message: "weekly limit reached",
		Err: &router.Failed{Entry: entry},
	}
	err := loop.StopFor(fmt.Errorf("stream: %w", perr))

	var stopped *loop.StopError
	require.ErrorAs(t, err, &stopped)
	assert.Equal(t, harness.Stop{
		Kind: harness.StopRateLimit, ResetAt: reset, RetryAfterSeconds: 90,
		Account: "work", Model: "opus", Message: "weekly limit reached",
	}, stopped.Stop)
	assert.ErrorIs(t, err, perr)

	missing := &models.NoModelError{}
	selection := fmt.Errorf("resolve: %w", missing)
	mapped := loop.StopFor(selection)
	require.ErrorAs(t, mapped, &stopped)
	assert.Equal(t, harness.StopRequest, stopped.Stop.Kind)
	assert.Equal(t, missing.Error(), stopped.Stop.Message)
	assert.ErrorIs(t, mapped, selection)
	var original *models.NoModelError
	require.ErrorAs(t, mapped, &original)
	assert.Same(t, missing, original)
	assert.Same(t, mapped, loop.StopFor(mapped), "mapping an existing stop must not wrap it again")

	plain := errors.New("disk full")
	assert.Equal(t, plain, loop.StopFor(plain), "an error that is not the provider's is not given a stop")
	unlisted := &provider.Error{Reason: "solar_flare"}
	assert.Equal(t, error(unlisted), loop.StopFor(unlisted), "a reason outside the list is not mapped to a guess")

	for _, reason := range []provider.Reason{
		provider.ReasonRateLimit, provider.ReasonAuth, provider.ReasonOverload,
		provider.ReasonContextFull, provider.ReasonNetwork, provider.ReasonRequest,
	} {
		require.ErrorAs(t, loop.StopFor(&provider.Error{Reason: reason}), &stopped, reason)
		// The two lists use the same words, which is what a client relies on.
		assert.Equal(t, string(reason), string(stopped.Stop.Kind))
	}
}

func TestConflicts(t *testing.T) {
	read := func(paths ...string) loop.Job { return loop.Job{Effect: tools.ReadOnly, ReadPaths: paths} }
	write := func(paths ...string) loop.Job { return loop.Job{Effect: tools.Writes, WritePaths: paths} }
	exec := loop.Job{Effect: tools.Executes}
	for _, tc := range []struct {
		name string
		a, b loop.Job
		want bool
	}{
		{"two reads of one file", read("/a"), read("/a"), false},
		{"writes of different files", write("/a"), write("/b"), false},
		{"writes that share one of their files", write("/a", "/b"), write("/b", "/c"), true},
		{"a read and a write of one file", read("/a"), write("/a"), true},
		{"a read and a write of different files", read("/a"), write("/b"), false},
		{"a write that does not say where", write(), write("/a"), true},
		{"a write that does not say where, and a read", write(), read("/a"), true},
		{"a command and a read", exec, read("/a"), true},
		{"two commands", exec, exec, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, loop.Conflicts(tc.a, tc.b))
			assert.Equal(t, tc.want, loop.Conflicts(tc.b, tc.a))
		})
	}
}

func TestSettle(t *testing.T) {
	done := tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: "wrote it"}}}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name      string
		effect    tools.Effect
		res       tools.Result
		err       error
		cancelled bool
		want      transcript.Outcome
	}{
		{"a result stands", tools.Writes, done, nil, false, transcript.OutcomeCompleted},
		{"a result stands after a cancel", tools.Executes, done, nil, true, transcript.OutcomeCompleted},
		{"a tool's own cancelled outcome stands", tools.Executes, tools.Result{Outcome: transcript.OutcomeCancelled, Parts: done.Parts}, nil, true, transcript.OutcomeCancelled},
		{"an error after a cancel, from a tool that writes", tools.Writes, tools.Result{}, boom, true, transcript.OutcomeUnknown},
		{"an error after a cancel, from a tool that executes", tools.Executes, tools.Result{}, boom, true, transcript.OutcomeUnknown},
		{"an error after a cancel, from a tool that reads", tools.ReadOnly, tools.Result{}, boom, true, transcript.OutcomeCancelled},
		{"an error without a cancel", tools.Writes, tools.Result{}, boom, false, transcript.OutcomeFailed},
		{"a result with no outcome", tools.Writes, tools.Result{}, nil, false, transcript.OutcomeFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := loop.Settle(tc.effect, tc.res, tc.err, tc.cancelled)
			assert.Equal(t, tc.want, got.Outcome)
			assert.NotEmpty(t, got.Parts, "the model is always told something")
		})
	}
}

func TestConversation(t *testing.T) {
	user := func(text string) transcript.Item {
		return transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}
	}
	callItem := func(id string) transcript.Item {
		return transcript.Item{ToolCall: &transcript.ToolCall{ID: id, Name: "write"}}
	}
	resultItem := func(id string) transcript.Item {
		return transcript.Item{ToolResult: &transcript.ToolResult{CallID: id, Name: "write", Outcome: transcript.OutcomeCompleted}}
	}
	text := transcript.Item{Text: &transcript.Text{Text: "done"}}

	t.Run("an interruption becomes a user message and bookkeeping is left out", func(t *testing.T) {
		items, repairs, err := loop.Conversation([]transcript.Item{
			user("go"), callItem("1"), resultItem("1"),
			{Usage: &transcript.Usage{Model: "m"}},
			{ModelSwitch: &transcript.ModelSwitch{From: "a", To: "b"}},
			{Interrupted: &transcript.Interrupted{}},
			user("again"), text,
		}, "[interrupted]")
		require.NoError(t, err)
		assert.Empty(t, repairs)
		assert.Equal(t, []transcript.Item{user("go"), callItem("1"), resultItem("1"), user("[interrupted]"), user("again"), text}, items)
	})

	t.Run("calls a dead process left without a result get the unknown outcome", func(t *testing.T) {
		items, repairs, err := loop.Conversation([]transcript.Item{
			user("go"), callItem("1"), callItem("2"), resultItem("1"),
		}, "[interrupted]")
		require.NoError(t, err)
		require.Len(t, repairs, 1)
		assert.Equal(t, "2", repairs[0].ToolResult.CallID)
		assert.Equal(t, transcript.OutcomeUnknown, repairs[0].ToolResult.Outcome)
		assert.Equal(t, repairs[0], items[len(items)-1], "the model is sent the result that is stored")
	})

	t.Run("developer interruption preserves surrounding user messages", func(t *testing.T) {
		before, after := user("go"), user("again")
		items, repairs, err := loop.ConversationWithRole([]transcript.Item{
			before, {Interrupted: &transcript.Interrupted{}}, after,
		}, "[interrupted]", "developer")
		require.NoError(t, err)
		assert.Empty(t, repairs)
		require.Len(t, items, 3)
		assert.Equal(t, before, items[0])
		assert.Equal(t, after, items[2])
		assert.Equal(t, "developer", items[1].User.Role)
		assert.Equal(t, "[interrupted]", items[1].User.Parts[0].Text)
	})

	t.Run("unsupported roles fail before preparing a request", func(t *testing.T) {
		_, _, err := loop.ConversationWithRole(nil, "[interrupted]", "system")
		require.Error(t, err)
		invalid := user("injected")
		invalid.User.Role = "assistant"
		_, _, err = loop.Conversation([]transcript.Item{invalid}, "[interrupted]")
		require.Error(t, err)
	})

	t.Run("a call without a result in the middle of a session is an error", func(t *testing.T) {
		_, _, err := loop.Conversation([]transcript.Item{user("go"), callItem("1"), user("again")}, "[interrupted]")
		assert.Error(t, err)
	})
}

func TestResponseShowsEachPieceOnce(t *testing.T) {
	var m loop.Mapper
	var r loop.Response
	var shown []string
	add := func(e provider.Event) {
		for _, u := range r.Add(m, e) {
			switch {
			case u.AgentMessageChunk != nil:
				shown = append(shown, "text:"+u.AgentMessageChunk.Content.Text.Text)
			case u.AgentThoughtChunk != nil:
				shown = append(shown, "thought:"+u.AgentThoughtChunk.Content.Text.Text)
			case u.ToolCall != nil:
				shown = append(shown, "call:"+string(u.ToolCall.ToolCallId))
			}
		}
	}
	add(provider.Event{ThinkingDelta: "hm"})
	add(provider.Event{Item: &transcript.Item{Thinking: &transcript.Thinking{Text: "hm"}}})
	add(provider.Event{TextDelta: "Hel"})
	add(provider.Event{TextDelta: "lo"})
	add(provider.Event{Item: &transcript.Item{Text: &transcript.Text{Text: "Hello"}}})
	// A provider that sends a finished text without deltas.
	add(provider.Event{Item: &transcript.Item{Text: &transcript.Text{Text: "whole"}}})
	add(provider.Event{Item: &transcript.Item{ToolCall: &transcript.ToolCall{ID: "c1", Name: "read"}}})
	add(provider.Event{Usage: &transcript.Usage{OutputTokens: 1}})
	add(provider.Event{Usage: &transcript.Usage{OutputTokens: 9}})
	add(provider.Event{Stop: provider.StopToolUse})

	assert.Equal(t, []string{"thought:hm", "text:Hel", "text:lo", "text:whole", "call:c1"}, shown)
	assert.Len(t, r.Items, 4)
	assert.EqualValues(t, 9, r.Usage.OutputTokens, "the last usage is the request's total")
	assert.Equal(t, provider.StopToolUse, r.Stop)
	require.Len(t, r.Calls(), 1)
}
