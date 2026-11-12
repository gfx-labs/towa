package loop

import (
	"errors"
	"fmt"
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
)

func result(call transcript.ToolCall, outcome transcript.Outcome, text string) transcript.ToolResult {
	return transcript.ToolResult{
		CallID: call.ID, Name: call.Name, Outcome: outcome, Parts: []transcript.Part{{Text: text}},
	}
}

// told returns a result's parts as a request can hold them. A part with
// nothing set is left out: a command that printed nothing returns empty
// text, and a provider refuses a part that holds nothing. A result left with
// no part says so in words.
func told(parts []transcript.Part) []transcript.Part {
	if len(parts) > 0 && !slices.Contains(parts, transcript.Part{}) {
		return parts
	}
	kept := slices.DeleteFunc(slices.Clone(parts), func(p transcript.Part) bool { return p == (transcript.Part{}) })
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
// stored with a part that holds nothing is sent as Settle would have left it.
func Conversation(history []transcript.Item, interruption string) (items []transcript.Item, repairs []transcript.Item, err error) {
	interrupted := transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: interruption}}}}
	if err := interrupted.Validate(); err != nil {
		return nil, nil, err
	}
	answered := map[string]bool{}
	for _, item := range history {
		if item.ToolResult != nil {
			answered[item.ToolResult.CallID] = true
		}
	}
	// A call is stored before it is dispatched and its result before
	// anything else, and every turn starts here. So a call without a result
	// can only be at the end, where its result is appended.
	var open []transcript.Item
	dangling := func() error {
		if len(open) == 0 {
			return nil
		}
		return fmt.Errorf("loop: tool call %s has no result and is not at the end of the session", open[0].ToolResult.CallID)
	}
	for _, item := range history {
		if err := item.Validate(); err != nil {
			return nil, nil, err
		}
		switch {
		case item.ToolCall != nil:
			items = append(items, item)
			if !answered[item.ToolCall.ID] {
				lost := result(*item.ToolCall, transcript.OutcomeUnknown, textLost)
				open = append(open, transcript.Item{ToolResult: &lost})
			}
		case item.Text != nil, item.Thinking != nil, item.Native != nil:
			items = append(items, item)
		case item.ToolResult != nil:
			result := *item.ToolResult
			result.Parts = told(result.Parts)
			item.ToolResult = &result
			items = append(items, item)
		case item.User != nil:
			if err := dangling(); err != nil {
				return nil, nil, err
			}
			items = append(items, item)
		case item.Interrupted != nil:
			if err := dangling(); err != nil {
				return nil, nil, err
			}
			items = append(items, interrupted)
		case item.Compaction != nil:
			return nil, nil, errors.New("loop: compaction must be materialized before Conversation")
		}
	}
	return append(items, open...), open, nil
}
