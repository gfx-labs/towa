// Package todo is the todo list capability: it checks one list from the
// model and returns it with the plan a client is shown. It holds no state,
// because the upstream tools it serves (Claude Code's TodoWrite and Codex's
// update_plan) send the whole list on every call. A dialect adapter turns
// the upstream input into a List and a Result or an *Error into the
// upstream tool's words.
package todo

import (
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/pkg/acp"
)

// Status is how far an item is.
type Status string

// Statuses of an item. Both upstream tools use these three words.
const (
	Pending    Status = "pending"
	InProgress Status = "in_progress"
	Completed  Status = "completed"
)

// Priority is how much an item matters.
type Priority string

// Priorities of an item.
const (
	High   Priority = "high"
	Medium Priority = "medium"
	Low    Priority = "low"
)

// Item is one entry of the list.
type Item struct {
	// Content says what is to be done.
	Content string
	Status  Status
	// Priority may be empty: neither upstream tool sends one today. The
	// plan needs a value, and an item without one is shown as Medium.
	Priority Priority
	// ActiveForm is the wording for the item while it is in progress
	// ("Running the tests"). It may be empty.
	ActiveForm string
}

// List is one call's input: the whole list, in order.
type List struct {
	Items []Item
	// Explanation says why the list changed. It may be empty.
	Explanation string
}

// Result is a list that passed the checks.
type Result struct {
	// Items is the list with surrounding white space trimmed.
	Items       []Item
	Explanation string
	// Entries is the list as a client is shown it. An item in progress
	// that has an active form is shown with that wording.
	Entries []acp.PlanEntry
	// InProgress is how many items are in progress. Neither upstream tool
	// was found to refuse a list with several (see the package's note), so
	// this package does not either. An adapter whose upstream does can
	// refuse on this count.
	InProgress int
}

// Kind says what is wrong with a list.
type Kind string

// Kinds of an Error.
const (
	// KindEmptyContent: the item has no content.
	KindEmptyContent Kind = "empty_content"
	// KindUnknownStatus: Value is not a Status.
	KindUnknownStatus Kind = "unknown_status"
	// KindUnknownPriority: Value is not empty and not a Priority.
	KindUnknownPriority Kind = "unknown_priority"
)

// Error is a list the model got wrong. Check returns no other error.
type Error struct {
	Kind Kind
	// Index is the position of the item in the list.
	Index int
	// Value is the status or priority that is not known.
	Value string
}

func (e *Error) Error() string {
	if e.Value == "" {
		return fmt.Sprintf("todo: %s: item %d", e.Kind, e.Index)
	}
	return fmt.Sprintf("todo: %s: item %d: %q", e.Kind, e.Index, e.Value)
}

// Check validates a list. An empty list is valid: it clears the plan.
func Check(list List) (Result, error) {
	r := Result{
		Items:       make([]Item, len(list.Items)),
		Explanation: strings.TrimSpace(list.Explanation),
		Entries:     make([]acp.PlanEntry, len(list.Items)),
	}
	for i, item := range list.Items {
		item.Content = strings.TrimSpace(item.Content)
		item.ActiveForm = strings.TrimSpace(item.ActiveForm)
		if item.Content == "" {
			return Result{}, &Error{Kind: KindEmptyContent, Index: i}
		}
		switch item.Status {
		case Pending, Completed:
		case InProgress:
			r.InProgress++
		default:
			return Result{}, &Error{Kind: KindUnknownStatus, Index: i, Value: string(item.Status)}
		}
		priority := item.Priority
		switch priority {
		case High, Medium, Low:
		case "":
			priority = Medium
		default:
			return Result{}, &Error{Kind: KindUnknownPriority, Index: i, Value: string(item.Priority)}
		}
		shown := item.Content
		if item.Status == InProgress && item.ActiveForm != "" {
			shown = item.ActiveForm
		}
		r.Items[i] = item
		r.Entries[i] = acp.PlanEntry{
			Content:  shown,
			Status:   acp.PlanEntryStatus(item.Status),
			Priority: acp.PlanEntryPriority(priority),
		}
	}
	return r, nil
}

// Update returns the session update that shows the entries to the client.
//
// With an id it is a plan_update, which lets a client hold several plans
// apart, when sendable says the client takes that type (loop.Turn.Sendable).
// Otherwise it is a plan, which every client takes and which replaces the
// session's one plan.
func Update(entries []acp.PlanEntry, id acp.PlanId, sendable func(acp.SessionUpdate) bool) acp.SessionUpdate {
	if id != "" {
		byID := acp.SessionUpdate{PlanUpdate: &acp.SessionPlanUpdate{
			SessionUpdate: "plan_update",
			Plan:          acp.NewPlanUpdateContentItems(id, entries),
		}}
		if sendable(byID) {
			return byID
		}
	}
	return acp.SessionUpdate{Plan: &acp.SessionUpdatePlan{SessionUpdate: "plan", Entries: entries}}
}
