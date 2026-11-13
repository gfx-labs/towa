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
// The text of the child's last response is not added: it is the call's
// result. Thoughts, tool inputs and outputs, diffs, plans, usage and retry
// reports are not shown. They are in the child's session.
//
// The loop sends a turn's updates one at a time, so a view needs no lock.
type view struct {
	progress func(ctx context.Context, content []acp.ToolCallContent) error
	text     strings.Builder
	titles   map[acp.ToolCallId]string
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
		var content []acp.ToolCallContent
		if v.text.Len() > 0 {
			content = append(content, tools.TextContent(v.text.String()))
			v.text.Reset()
		}
		if v.titles == nil {
			v.titles = map[acp.ToolCallId]string{}
		}
		v.titles[u.ToolCall.ToolCallId] = u.ToolCall.Title
		return v.progress(ctx, append(content, tools.TextContent(u.ToolCall.Title)))
	case u.ToolCallUpdate != nil:
		status := u.ToolCallUpdate.Status
		if status == nil || *status != acp.ToolCallStatusFailed {
			return nil
		}
		line := "failed: " + v.titles[u.ToolCallUpdate.ToolCallId]
		return v.progress(ctx, []acp.ToolCallContent{tools.TextContent(line)})
	}
	return nil
}
