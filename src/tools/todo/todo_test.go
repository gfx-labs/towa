package todo_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/tools/todo"
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

	// An empty list clears the plan. The update must still carry the
	// entries member, which the schema requires.
	empty, err := todo.Check(todo.List{})
	require.NoError(t, err)
	update := todo.Update(empty.Entries, "", session.NewGate(acp.ClientCapabilities{}).Sendable)
	encoded, err := json.Marshal(update)
	require.NoError(t, err)
	require.JSONEq(t, `{"sessionUpdate":"plan","entries":[]}`, string(encoded))
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

// The gate is the session's real one: what Turn.Sendable is.
func TestUpdatePicksTheTypeTheClientTakes(t *testing.T) {
	entries := []acp.PlanEntry{{Content: "Fix the bug", Status: acp.PlanEntryStatusPending, Priority: acp.PlanEntryPriorityMedium}}
	old := session.NewGate(acp.ClientCapabilities{}).Sendable
	withPlans := session.NewGate(acp.ClientCapabilities{Plan: &acp.PlanCapabilities{}}).Sendable

	byID := todo.Update(entries, "main", withPlans)
	require.Equal(t, acp.PlanId("main"), byID.PlanUpdate.Plan.Items.PlanId)
	require.Equal(t, entries, byID.PlanUpdate.Plan.Items.Entries)

	for name, update := range map[string]acp.SessionUpdate{
		"a client that did not advertise plan updates": todo.Update(entries, "main", old),
		"no plan id": todo.Update(entries, "", withPlans),
	} {
		require.Nil(t, update.PlanUpdate, name)
		require.Equal(t, entries, update.Plan.Entries, name)
	}
}
