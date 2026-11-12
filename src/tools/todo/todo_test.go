package todo_test

import (
	"encoding/json/v2"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/tools/todo"
	"github.com/gfx-labs/acpharness/src/transcript"
)

func TestCheckReturnsTheListAndItsPlan(t *testing.T) {
	got, err := todo.Check(todo.List{
		Explanation: " tests first ",
		Items: []todo.Item{
			{Content: "Read the parser", Status: todo.Completed, ActiveForm: "Reading the parser"},
			{Content: " Run the tests ", Status: todo.InProgress, ActiveForm: "Running the tests", Priority: todo.High},
			{Content: "Fix the bug", Status: todo.InProgress},
			{Content: "Write the note", Status: todo.Pending, Priority: todo.Low},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "tests first", got.Explanation)
	require.Equal(t, "Run the tests", got.Items[1].Content)
	// Two items in progress are counted and not refused.
	require.Equal(t, 2, got.InProgress)
	require.Equal(t, []acp.PlanEntry{
		{Content: "Read the parser", Status: acp.PlanEntryStatusCompleted, Priority: acp.PlanEntryPriorityMedium},
		{Content: "Running the tests", Status: acp.PlanEntryStatusInProgress, Priority: acp.PlanEntryPriorityHigh},
		{Content: "Fix the bug", Status: acp.PlanEntryStatusInProgress, Priority: acp.PlanEntryPriorityMedium},
		{Content: "Write the note", Status: acp.PlanEntryStatusPending, Priority: acp.PlanEntryPriorityLow},
	}, got.Entries)
}

func TestCheckRefusesWhatTheModelGotWrong(t *testing.T) {
	ok := todo.Item{Content: "Fix the bug", Status: todo.Pending}
	tests := []struct {
		name string
		item todo.Item
		want todo.Error
	}{
		{"content of white space", todo.Item{Content: " \n", Status: todo.Pending}, todo.Error{Kind: todo.KindEmptyContent, Index: 1}},
		{"a status of another tool", todo.Item{Content: "x", Status: "done"}, todo.Error{Kind: todo.KindUnknownStatus, Index: 1, Value: "done"}},
		{"no status", todo.Item{Content: "x"}, todo.Error{Kind: todo.KindUnknownStatus, Index: 1}},
		{"an unknown priority", todo.Item{Content: "x", Status: todo.Pending, Priority: "urgent"}, todo.Error{Kind: todo.KindUnknownPriority, Index: 1, Value: "urgent"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := todo.Check(todo.List{Items: []todo.Item{ok, tt.item}})
			var got *todo.Error
			require.ErrorAs(t, err, &got)
			require.Equal(t, tt.want, *got)
		})
	}
}

// log builds a session's entries: a string is a user message, a list is an
// accepted todo call with those items, each "content:status".
func log(t *testing.T, steps ...any) []transcript.Entry {
	t.Helper()
	var entries []transcript.Entry
	add := func(item transcript.Item) { entries = append(entries, transcript.Entry{Item: item}) }
	for i, step := range steps {
		switch v := step.(type) {
		case string:
			add(transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: v}}}})
		case []string:
			items := make([]map[string]string, len(v))
			for j, s := range v {
				content, status, _ := strings.Cut(s, ":")
				items[j] = map[string]string{"content": content, "status": status}
			}
			input, err := json.Marshal(map[string]any{"todos": items})
			require.NoError(t, err)
			id := fmt.Sprint("call", i)
			add(transcript.Item{ToolCall: &transcript.ToolCall{ID: id, Name: todo.Name, Input: input}})
			add(transcript.Item{ToolResult: &transcript.ToolResult{CallID: id, Name: todo.Name, Outcome: transcript.OutcomeCompleted}})
		}
	}
	return entries
}

func TestOpenReadsTheLastAcceptedListAndItsPokes(t *testing.T) {
	first := []string{"read:completed", "fix:in_progress", "test:pending"}
	left := []string{"fix", "test"}
	open, sent, err := todo.Open(log(t, "do it", first))
	require.NoError(t, err)
	require.Equal(t, left, open)
	require.Zero(t, sent)

	// Pokes for the same unfinished items add up, also when the model
	// sends the list again without finishing anything.
	open, sent, err = todo.Open(log(t, "do it", first, todo.Poke(left), first, todo.Poke(left)))
	require.NoError(t, err)
	require.Equal(t, left, open)
	require.Equal(t, 2, sent)

	// Progress starts the count again, and so does the user writing.
	moved := []string{"read:completed", "fix:completed", "test:pending"}
	_, sent, err = todo.Open(log(t, "do it", first, todo.Poke(left), moved))
	require.NoError(t, err)
	require.Zero(t, sent)
	_, sent, err = todo.Open(log(t, "do it", first, todo.Poke(left), "carry on"))
	require.NoError(t, err)
	require.Zero(t, sent)

	// A finished list, a refused call and a session without the tool leave
	// nothing to go back to.
	open, _, err = todo.Open(log(t, "do it", first, []string{"read:completed"}))
	require.NoError(t, err)
	require.Empty(t, open)
	refused := log(t, "do it", first)
	refused[len(refused)-1].Item.ToolResult.Outcome = transcript.OutcomeFailed
	open, _, err = todo.Open(refused)
	require.NoError(t, err)
	require.Empty(t, open)
	open, _, err = todo.Open(log(t, "hello"))
	require.NoError(t, err)
	require.Empty(t, open)
}
