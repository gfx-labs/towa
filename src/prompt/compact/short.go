package compact

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/gfx-labs/towa/src/transcript"
)

// The short prompt is jcode's, copied byte for byte from
// github.com/1jehuang/jcode at origin/master
// 8e11eb23ea33c7b09d43aa9a3b52fa3f0728f39e.

// shortSystem is the system prompt of the summary call in
// generate_compaction_artifact, crates/jcode-base/src/compaction.rs:1799.
const shortSystem = "You are a helpful assistant that summarizes conversations."

// shortPrompt and searchSentence, joined by one space, are SUMMARY_PROMPT
// in crates/jcode-compaction-core/src/lib.rs:85. The prompt's last sentence
// is kept apart because it is true only when the conversation_search tool
// is enabled.
const (
	shortPrompt = `Summarize our conversation so you can continue this work later.

Write in natural language with these sections:
- **Context:** What we're working on and why (1-2 sentences)
- **What we did:** Key actions taken, files changed, problems solved
- **Current state:** What works, what's broken, what's next
- **User preferences:** Specific requirements or decisions they made

Be concise but preserve important details.`
	searchSentence = `You can search the full conversation later if you need exact error messages or code snippets.`
)

// shortResultBytes is how much of a tool result jcode's summary request
// shows, in bytes: build_compaction_conversation_text,
// crates/jcode-compaction-core/src/lib.rs:217.
const shortResultBytes = 500

// shortText follows build_compaction_prompt and
// build_compaction_conversation_text in jcode's
// crates/jcode-compaction-core/src/lib.rs.
func shortText(in SummaryInput) string {
	return shortWrap(in, shortConversation(in.Replaced))
}

func shortConversation(replaced []transcript.Entry) string {
	var b strings.Builder
	for _, m := range spans(replaced) {
		if m.role == roleUser {
			b.WriteString("**User:**\n")
		} else {
			b.WriteString("**Assistant:**\n")
		}
		for _, e := range replaced[m.start:m.end] {
			writeShortItem(&b, e.Item)
		}
		b.WriteString("\n")
	}
	return b.String()
}

func writeShortItem(b *strings.Builder, item transcript.Item) {
	switch {
	case item.User != nil:
		if text := partsText(item.User.Parts, "[Image]"); text != "" {
			b.WriteString(text)
			b.WriteString("\n")
		}
	case item.Text != nil:
		b.WriteString(item.Text.Text)
		b.WriteString("\n")
	case item.ToolCall != nil:
		fmt.Fprintf(b, "[Tool: %s - %s]\n", item.ToolCall.Name, item.ToolCall.Input)
	case item.ToolResult != nil:
		text := partsText(item.ToolResult.Parts, "[Image]")
		if len(text) > shortResultBytes {
			text = cutBytes(text, shortResultBytes) + "... (truncated)"
		}
		fmt.Fprintf(b, "[Result: %s]\n", text)
	}
}

func shortWrap(in SummaryInput, conversation string) string {
	var b strings.Builder
	if in.Previous != "" {
		fmt.Fprintf(&b, "## Previous Summary\n\n%s\n\n## New Conversation\n\n", in.Previous)
	}
	b.WriteString(conversation)
	b.WriteString("\n\n---\n\n" + shortPrompt)
	if in.ConversationSearch {
		b.WriteString(" " + searchSentence)
	}
	return b.String()
}

// cutBytes keeps the first limit bytes of text, back to a character
// boundary, as jcode's truncate_str_boundary does.
func cutBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
