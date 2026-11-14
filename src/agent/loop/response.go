package loop

import (
	"slices"
	"strings"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// StopReason is the ACP stop reason for a response the model ended. It
// reports false for tool use, which does not end a turn, and for a reason
// outside provider's list.
func StopReason(stop provider.StopReason) (acp.StopReason, bool) {
	switch stop {
	case provider.StopEndTurn:
		return acp.StopReasonEndTurn, true
	case provider.StopMaxTokens:
		return acp.StopReasonMaxTokens, true
	case provider.StopRefusal:
		return acp.StopReasonRefusal, true
	}
	return "", false
}

// Response collects one model response from its events.
type Response struct {
	// Items are the finished items, in order.
	Items []transcript.Item
	// Usage is the last usage reported, nil when none was.
	Usage *transcript.Usage
	// Stop is empty until the last event.
	Stop provider.StopReason

	// textShown and thoughtShown say a delta was sent since the last
	// finished item of that kind.
	textShown, thoughtShown bool
	// writing holds the ids of the calls announced while the model was
	// still writing them, in order, until each one finishes.
	writing []string
	// arrived says an event came. Before one does, the router owns retrying.
	arrived bool
}

// Add takes one event and returns the updates that show it to the client.
// A finished text or thinking item is shown only when no delta came for it,
// since its deltas already showed it.
func (r *Response) Add(m Mapper, e provider.Event) []acp.SessionUpdate {
	r.arrived = true
	switch {
	case e.TextDelta != "":
		r.textShown = true
		return []acp.SessionUpdate{TextChunk(e.TextDelta)}
	case e.ThinkingDelta != "":
		r.thoughtShown = true
		return []acp.SessionUpdate{ThoughtChunk(e.ThinkingDelta)}
	case e.ToolCall != nil:
		return r.progress(m, *e.ToolCall)
	case e.Item != nil:
		item := *e.Item
		r.Items = append(r.Items, item)
		switch {
		case item.Text != nil:
			shown := r.textShown
			r.textShown = false
			if !shown && item.Text.Text != "" {
				return []acp.SessionUpdate{TextChunk(item.Text.Text)}
			}
		case item.Thinking != nil:
			shown := r.thoughtShown
			r.thoughtShown = false
			if !shown && item.Thinking.Text != "" {
				return []acp.SessionUpdate{ThoughtChunk(item.Thinking.Text)}
			}
		case item.ToolCall != nil:
			call := m.ToolCall(*item.ToolCall)
			i := slices.Index(r.writing, item.ToolCall.ID)
			if i < 0 {
				return []acp.SessionUpdate{{ToolCall: &call}}
			}
			// The call was announced under its name. Now that its argument
			// is whole it gets its title, kind, locations and input.
			r.writing = slices.Delete(r.writing, i, i+1)
			return []acp.SessionUpdate{{ToolCallUpdate: &acp.SessionToolCallUpdate{
				ToolCallId: call.ToolCallId, Title: &call.Title, Kind: &call.Kind,
				Locations: call.Locations, RawInput: call.RawInput,
			}}}
		case item.Native != nil:
			return HostedSearch(item)
		}
	case e.Usage != nil:
		r.Usage = e.Usage
	case e.Stop != "":
		r.Stop = e.Stop
	}
	return nil
}

// progress announces a call the model has started writing, once, when it
// has an id and a name. The pieces of its argument are not shown: ACP has
// no update for part of an argument, and the whole one follows. The tool
// describes the call with no input yet, so the client sees its kind and
// title from the first frame.
func (r *Response) progress(m Mapper, p provider.ToolCallProgress) []acp.SessionUpdate {
	if p.ID == "" || p.Name == "" || slices.Contains(r.writing, p.ID) {
		return nil
	}
	r.writing = append(r.writing, p.ID)
	call := m.ToolCall(transcript.ToolCall{ID: p.ID, Name: p.Name})
	return []acp.SessionUpdate{{ToolCall: &call}}
}

// Writing returns the ids of the calls that were announced and never
// finished, in the order they were announced.
func (r *Response) Writing() []string {
	return r.writing
}

// Text returns the response's text items joined.
func (r *Response) Text() string {
	var text strings.Builder
	for _, item := range r.Items {
		if item.Text != nil {
			text.WriteString(item.Text.Text)
		}
	}
	return text.String()
}

// Calls returns the response's tool calls in order.
func (r *Response) Calls() []transcript.ToolCall {
	var calls []transcript.ToolCall
	for _, item := range r.Items {
		if item.ToolCall != nil {
			calls = append(calls, *item.ToolCall)
		}
	}
	return calls
}
