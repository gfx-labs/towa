package subagent

import (
	"context"
	"strings"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/tools"
)

// view shows a child's turn under the parent's tool call. A tool call update
// carries a list of content blocks and nothing nested, and the loop sends
// the whole list again with every addition. So the view adds one block per
// event worth a line, never one per streamed piece:
//
//   - the text the child wrote before a tool call, as one block when the
//     call starts,
//   - one line with the title of each tool call the child starts,
//   - one line for each of them that fails.
//
// A call the child's model is still writing is announced under its tool's
// name and gets its title when its argument is whole. Its line is added
// then, so that it carries the title.
//
// The text of the child's last response is not added: it is the call's
// result. Thoughts, tool inputs and outputs, diffs, plans, usage and retry
// reports are not shown. They are in the child's session.
//
// The loop sends a turn's updates one at a time, so a view needs no lock.
type view struct {
	progress func(ctx context.Context, content []acp.ToolCallContent) error
	text     strings.Builder
	titles   map[acp.ToolCallId]string
	// waiting holds the calls announced without their argument, which have
	// no line yet.
	waiting map[acp.ToolCallId]bool
}

func (v *view) update(ctx context.Context, u acp.SessionUpdate) error {
	if v.progress == nil {
		return nil
	}
	switch {
	case u.AgentMessageChunk != nil:
		if text := u.AgentMessageChunk.Content.Text; text != nil {
			v.text.WriteString(text.Text)
		}
	case u.ToolCall != nil:
		id := u.ToolCall.ToolCallId
		if v.titles == nil {
			v.titles = map[acp.ToolCallId]string{}
			v.waiting = map[acp.ToolCallId]bool{}
		}
		v.titles[id] = u.ToolCall.Title
		if u.ToolCall.RawInput == nil {
			v.waiting[id] = true
			return nil
		}
		return v.started(ctx, id)
	case u.ToolCallUpdate != nil:
		id := u.ToolCallUpdate.ToolCallId
		if title := u.ToolCallUpdate.Title; title != nil {
			v.titles[id] = *title
		}
		status := u.ToolCallUpdate.Status
		if v.waiting[id] {
			if err := v.started(ctx, id); err != nil {
				return err
			}
		}
		if status == nil || *status != acp.ToolCallStatusFailed {
			return nil
		}
		line := "failed: " + v.titles[id]
		return v.progress(ctx, []acp.ToolCallContent{tools.TextContent(line)})
	}
	return nil
}

// started adds the line of a call, after the text the child wrote before it.
func (v *view) started(ctx context.Context, id acp.ToolCallId) error {
	delete(v.waiting, id)
	var content []acp.ToolCallContent
	if v.text.Len() > 0 {
		content = append(content, tools.TextContent(v.text.String()))
		v.text.Reset()
	}
	return v.progress(ctx, append(content, tools.TextContent(v.titles[id])))
}
