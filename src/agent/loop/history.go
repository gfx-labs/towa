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
	interrupted := transcript.Item{Body: transcript.User{Parts: []transcript.Part{{Text: interruption}}}}
	if err := interrupted.Validate(); err != nil {
		return nil, nil, err
	}
	answered := map[string]bool{}
	for _, item := range history {
		if result, ok := item.Body.(transcript.ToolResult); ok {
			answered[result.CallID] = true
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
		lost, _ := open[0].Body.(transcript.ToolResult)
		return fmt.Errorf("loop: tool call %s has no result and is not at the end of the session", lost.CallID)
	}
	for _, item := range history {
		if err := item.Validate(); err != nil {
			return nil, nil, err
		}
		switch body := item.Body.(type) {
		case transcript.ToolCall:
			items = append(items, item)
			if !answered[body.ID] {
				lost := result(body, transcript.OutcomeUnknown, textLost)
				open = append(open, transcript.Item{Body: lost})
			}
		case transcript.Text, transcript.Thinking, transcript.Native:
			items = append(items, item)
		case transcript.ToolResult:
			result := body
			result.Parts = told(result.Parts)
			item.Body = result
			items = append(items, item)
		case transcript.User:
			if err := dangling(); err != nil {
				return nil, nil, err
			}
			user := body
			user.Parts = filled(user.Parts)
			if len(user.Parts) == 0 {
				continue
			}
			item.Body = user
			items = append(items, item)
		case transcript.Interrupted:
			if err := dangling(); err != nil {
				return nil, nil, err
			}
			items = append(items, interrupted)
		case transcript.Compaction:
			return nil, nil, errors.New("loop: compaction must be materialized before Conversation")
		case transcript.Usage, transcript.ModelSwitch:
			// Not part of the conversation.
		}
	}
	return append(items, open...), open, nil
}
