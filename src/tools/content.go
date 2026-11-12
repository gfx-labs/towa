package tools

import "github.com/gfx-labs/towa/pkg/acp"

// TextContent returns text as content for the client.
func TextContent(text string) acp.ToolCallContent {
	return acp.ToolCallContent{Content: &acp.ToolCallContentContent{
		Type:    "content",
		Content: acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: text}},
	}}
}
