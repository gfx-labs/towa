package loop

import (
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/gfx-labs/towa/src/transcript"
)

// Texts the model gets for a call the loop answered itself.
const (
	textInterrupted  = "The tool call was cancelled before it ran."
	textNotRun       = "The tool call was not run because the model's response did not finish."
	textRejected     = "The user rejected this tool call."
	textUnknown      = "The tool call was interrupted while it ran. It may or may not have taken effect. Check before running it again."
	textReadStopped  = "The tool call was cancelled while it ran. It only reads, so it changed nothing."
	textLost         = "The harness stopped while this tool call was in flight. It may or may not have taken effect. Check before running it again."
	textNoOutcome    = "The tool returned a result without an outcome."
	textNoOutput     = "The tool call returned no output."
	textUnknownTool  = "There is no tool named %q."
	textBadInput     = "The tool call's input was not accepted: %v"
	textHarnessError = "The tool could not be run: %v"
	textBadHookInput = "A hook changed the tool call's input to one that was not accepted: %v"
	textNested       = "A %s call cannot hold another one."
	textLimit        = "The tool call was not run: the turn reached %s."
	// jobsPrefix starts the message that tells the model how background
	// commands ended. It is the harness speaking, as the todo poke is.
	jobsPrefix = "[auto] Background commands ended:\n\n"
)

func result(call transcript.ToolCall, outcome transcript.Outcome, text string) transcript.ToolResult {
	return transcript.ToolResult{
		CallID: call.ID, Name: call.Name, Outcome: outcome, Parts: []transcript.Part{{Text: text}},
	}
}

// userText is a user message of one text part.
func userText(text string) transcript.Item {
	return transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}
}

// filled returns parts without the ones that have nothing set, which a
// provider refuses.
func filled(parts []transcript.Part) []transcript.Part {
	if !slices.Contains(parts, transcript.Part{}) {
		return parts
	}
	return slices.DeleteFunc(slices.Clone(parts), func(p transcript.Part) bool { return p == (transcript.Part{}) })
}

// told returns a result's parts as a request can hold them. A part with
// nothing set is left out: a command that printed nothing returns empty
// text. A result left with no part says so in words.
func told(parts []transcript.Part) []transcript.Part {
	kept := filled(parts)
	if len(kept) == 0 {
		return []transcript.Part{{Text: textNoOutput}}
	}
	return kept
}

// Conversation prepares the items returned by compact.Items for a model
// request. It returns the items a provider accepts, and the results to store
// first for calls that have none.
//
// An Interrupted item becomes a user message with the interruption text.
// Usage and ModelSwitch items are not part of the conversation. A call
// without a result is one a process died on: nothing says whether it took
// effect, so it gets the unknown outcome and is not run again. A result
// stored with a part that holds nothing is sent as Settle would have left it,
// and a user message stored with one as UserItem would have: without the
// part, and not at all when no part is left.
func Conversation(history []transcript.Item, interruption string) (items []transcript.Item, repairs []transcript.Item, err error) {
	items = make([]transcript.Item, 0, len(history)+1)
	repairs, err = walk(slices.Values(history), interruption, func(item transcript.Item) { items = append(items, item) })
	if err != nil {
		return nil, nil, err
	}
	return append(items, repairs...), repairs, nil
}

// conversationInPlace is Conversation writing the items over history,
// which the caller owns and does not read again. Each item emitted takes
// the place of one read, so the writes never pass the reads.
func conversationInPlace(history []transcript.Item, interruption string) (items []transcript.Item, repairs []transcript.Item, err error) {
	n := 0
	repairs, err = walk(slices.Values(history), interruption, func(item transcript.Item) {
		history[n] = item
		n++
	})
	if err != nil {
		return nil, nil, err
	}
	return append(history[:n], repairs...), repairs, nil
}

// lostResults returns the results Conversation would store first, without
// building the items. A turn needs only these when it loads the session, and
// reads them from its entries so that it keeps no second list of the items.
func lostResults(entries []transcript.Entry, interruption string) ([]transcript.Item, error) {
	return walk(func(yield func(transcript.Item) bool) {
		for i := range entries {
			if entries[i].Item.Compaction == nil && !yield(entries[i].Item) {
				return
			}
		}
	}, interruption, nil)
}

// walk passes each item a request holds to emit, which may be nil, and
// returns the results for calls left without one.
func walk(history iter.Seq[transcript.Item], interruption string, emit func(transcript.Item)) ([]transcript.Item, error) {
	interrupted := userText(interruption)
	if err := interrupted.Validate(); err != nil {
		return nil, err
	}
	if emit == nil {
		emit = func(transcript.Item) {}
	}
	// A call is stored before it is dispatched and its result before
	// anything else, and every turn starts here. So a call without a result
	// can only be at the end, where its result is appended. pending holds
	// the calls without a result so far: few, since results follow their
	// calls.
	type call struct {
		item transcript.Item
		// followed says a user message or an interruption came after it.
		followed bool
	}
	var pending []call
	follow := func() {
		for i := range pending {
			pending[i].followed = true
		}
	}
	for item := range history {
		if err := item.Validate(); err != nil {
			return nil, err
		}
		switch {
		case item.ToolCall != nil:
			emit(item)
			pending = append(pending, call{item: item})
		case item.Text != nil, item.Thinking != nil, item.Native != nil:
			emit(item)
		case item.ToolResult != nil:
			pending = slices.DeleteFunc(pending, func(c call) bool { return c.item.ToolCall.ID == item.ToolResult.CallID })
			// The stored item is shared with the turn's entries: a changed
			// result is a copy.
			if parts := told(item.ToolResult.Parts); !sameParts(parts, item.ToolResult.Parts) {
				result := *item.ToolResult
				result.Parts = parts
				item.ToolResult = &result
			}
			emit(item)
		case item.User != nil:
			follow()
			parts := filled(item.User.Parts)
			if len(parts) == 0 {
				continue
			}
			if !sameParts(parts, item.User.Parts) {
				user := *item.User
				user.Parts = parts
				item.User = &user
			}
			emit(item)
		case item.Interrupted != nil:
			follow()
			emit(interrupted)
		case item.Compaction != nil:
			return nil, errors.New("loop: compaction must be materialized before Conversation")
		}
	}
	var repairs []transcript.Item
	for _, c := range pending {
		if c.followed {
			return nil, fmt.Errorf("loop: tool call %s has no result and is not at the end of the session", c.item.ToolCall.ID)
		}
		lost := result(*c.item.ToolCall, transcript.OutcomeUnknown, textLost)
		repairs = append(repairs, transcript.Item{ToolResult: &lost})
	}
	return repairs, nil
}

// sameParts reports whether a and b are the same slice.
func sameParts(a, b []transcript.Part) bool {
	return len(a) == len(b) && (len(a) == 0 || &a[0] == &b[0])
}
