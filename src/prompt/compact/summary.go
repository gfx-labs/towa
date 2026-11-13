package compact

import (
	"bytes"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// SummaryInput is what a summary request is built from.
type SummaryInput struct {
	// Prompt is config.SummaryCheckpoint or config.SummaryShort.
	Prompt string
	// Model, Credential, MaxOutputTokens and Effort go into the request as
	// they are. They belong to the model that writes the summary.
	Model           string
	Credential      auth.Credential
	MaxOutputTokens int64
	Effort          string
	// Replaced holds the entries the summary stands for: the leading part
	// of the active entries, up to the count Cut returned.
	Replaced []transcript.Entry
	// Previous is the summary of the compaction before this one, or empty.
	// The new summary has to carry it forward, because that compaction's
	// entry is no longer sent.
	Previous string
	// ConversationSearch says the conversation_search tool is enabled, so
	// the model may be told that it can look up what the summary leaves
	// out.
	ConversationSearch bool
}

// SummaryRequest builds the request that asks a model to summarise
// in.Replaced. The conversation is sent as text inside one user message, so
// that the model does not carry on with it, and the request has no tools.
func SummaryRequest(in SummaryInput) (provider.Request, error) {
	if len(in.Replaced) == 0 {
		return provider.Request{}, errors.New("compact: there are no entries to summarise")
	}
	var system, text string
	switch in.Prompt {
	case config.SummaryCheckpoint:
		system, text = checkpointSystem, checkpointText(in)
	case config.SummaryShort:
		system, text = shortSystem, shortText(in)
	default:
		return provider.Request{}, fmt.Errorf("compact: summary prompt %q is not %q or %q",
			in.Prompt, config.SummaryCheckpoint, config.SummaryShort)
	}
	return provider.Request{
		Model:           in.Model,
		Credential:      in.Credential,
		System:          []provider.SystemBlock{{Name: "compaction", Text: system}},
		Items:           []transcript.Item{{User: &transcript.User{Parts: []transcript.Part{{Text: text}}}}},
		MaxOutputTokens: in.MaxOutputTokens,
		Effort:          in.Effort,
	}, nil
}

// checkpointText follows generateSummaryWithUsage and serializeConversation
// in Pi's compaction.ts and utils.ts. Pi has no sentence about searching:
// jcode's is added after the prompt when the tool is enabled.
func checkpointText(in SummaryInput) string {
	return checkpointWrap(in, checkpointConversation(in.Replaced))
}

func checkpointConversation(replaced []transcript.Entry) string {
	var parts []string
	for _, m := range spans(replaced) {
		entries := replaced[m.start:m.end]
		if m.role == roleUser {
			for _, e := range entries {
				switch {
				case e.Item.User != nil:
					if text := partsText(e.Item.User.Parts, ""); text != "" {
						parts = append(parts, "[User]: "+text)
					}
				case e.Item.ToolResult != nil:
					if text := partsText(e.Item.ToolResult.Parts, ""); text != "" {
						parts = append(parts, "[Tool result]: "+cutUTF16(text, checkpointResultChars))
					}
				}
			}
			continue
		}
		var texts, calls []string
		for _, e := range entries {
			switch {
			case e.Item.Text != nil:
				texts = append(texts, e.Item.Text.Text)
			case e.Item.ToolCall != nil:
				calls = append(calls, e.Item.ToolCall.Name+"("+callArguments(e.Item.ToolCall.Input)+")")
			}
		}
		if len(texts) > 0 {
			parts = append(parts, "[Assistant]: "+strings.Join(texts, "\n"))
		}
		if len(calls) > 0 {
			parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
		}
	}
	return strings.Join(parts, "\n\n")
}

func checkpointWrap(in SummaryInput, conversation string) string {
	var b strings.Builder
	b.WriteString("<conversation>\n" + conversation + "\n</conversation>\n\n")
	prompt := checkpointPrompt
	if in.Previous != "" {
		b.WriteString("<previous-summary>\n" + in.Previous + "\n</previous-summary>\n\n")
		prompt = checkpointMergePrompt
	}
	b.WriteString(prompt)
	if in.ConversationSearch {
		b.WriteString("\n\n" + searchSentence)
	}
	return b.String()
}

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
			switch {
			case e.Item.User != nil:
				if text := partsText(e.Item.User.Parts, "[Image]"); text != "" {
					b.WriteString(text + "\n")
				}
			case e.Item.Text != nil:
				b.WriteString(e.Item.Text.Text + "\n")
			case e.Item.ToolCall != nil:
				fmt.Fprintf(&b, "[Tool: %s - %s]\n", e.Item.ToolCall.Name, e.Item.ToolCall.Input)
			case e.Item.ToolResult != nil:
				text := partsText(e.Item.ToolResult.Parts, "[Image]")
				if len(text) > shortResultBytes {
					text = cutBytes(text, shortResultBytes) + "... (truncated)"
				}
				fmt.Fprintf(&b, "[Result: %s]\n", text)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

func shortWrap(in SummaryInput, conversation string) string {
	var b strings.Builder
	if in.Previous != "" {
		b.WriteString("## Previous Summary\n\n" + in.Previous + "\n\n## New Conversation\n\n")
	}
	b.WriteString(conversation)
	b.WriteString("\n\n---\n\n" + shortPrompt)
	if in.ConversationSearch {
		b.WriteString(" " + searchSentence)
	}
	return b.String()
}

// partsText joins the text of parts with newlines. An image becomes image
// when that is not empty. A Stored part is never sent to a model.
func partsText(parts []transcript.Part, image string) string {
	var out []string
	for _, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			out = append(out, p.Text)
		case p.Image != nil:
			if image != "" {
				out = append(out, image)
			}
		case p.Resource != nil:
			if p.Resource.Text != "" {
				out = append(out, p.Resource.Text)
			} else if image != "" && strings.HasPrefix(p.Resource.MimeType, "image/") {
				out = append(out, image)
			}
		case p.Link != nil:
			out = append(out, fmt.Sprintf("[%s](%s)", p.Link.Name, p.Link.URI))
		}
	}
	return strings.Join(out, "\n")
}

// callArguments writes a tool call's input as Pi does: key=value pairs in
// the order the model wrote them, each value as JSON. An input that is not
// an object, such as a free-text tool's string, is written whole.
func callArguments(input jsontext.Value) string {
	dec := jsontext.NewDecoder(bytes.NewReader(input))
	if dec.PeekKind() != '{' {
		return strings.TrimSpace(string(input))
	}
	if _, err := dec.ReadToken(); err != nil {
		return strings.TrimSpace(string(input))
	}
	var pairs []string
	for dec.PeekKind() == '"' {
		name, err := dec.ReadToken()
		if err != nil {
			break
		}
		// The token is only valid until the decoder is used again.
		key := name.String()
		value, err := dec.ReadValue()
		if err != nil {
			break
		}
		value = value.Clone()
		if err := value.Compact(); err != nil {
			break
		}
		pairs = append(pairs, key+"="+string(value))
	}
	return strings.Join(pairs, ", ")
}

// cutUTF16 keeps the first limit UTF-16 units of text and says how many
// were left out, as Pi's truncateForSummary does. A surrogate pair is not
// split.
func cutUTF16(text string, limit int) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= limit {
		return text
	}
	end := limit
	if end > 0 && utf16.IsSurrogate(rune(units[end-1])) && utf16.IsSurrogate(rune(units[end])) && units[end-1] < 0xDC00 {
		end--
	}
	return fmt.Sprintf("%s\n\n[... %d more characters truncated]", string(utf16.Decode(units[:end])), len(units)-limit)
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

// Errors of a summary response that cannot stand for the entries it would
// replace. The entries are left as they are.
var (
	// ErrSummaryCutOff: the model hit its output limit, so the summary is
	// incomplete.
	ErrSummaryCutOff = errors.New("compact: the summary was cut off at the model's output limit")
	// ErrSummaryRefused: the model refused to write one.
	ErrSummaryRefused = errors.New("compact: the model refused to write a summary")
	// ErrSummaryToolCall: the model called a tool in place of answering.
	ErrSummaryToolCall = errors.New("compact: the model called a tool in place of writing a summary")
)

// SummaryText returns the summary from the finished items and the stop
// reason of a summary request's response. It refuses a response that is
// empty, cut off, refused or a tool call, as Pi does
// (getSummarizationFailure) and jcode does (validate_compaction_summary).
// It does not judge the wording.
func SummaryText(items []transcript.Item, stop provider.StopReason) (string, error) {
	var texts []string
	for _, item := range items {
		switch {
		case item.ToolCall != nil:
			return "", ErrSummaryToolCall
		case item.Text != nil:
			texts = append(texts, item.Text.Text)
		}
	}
	switch stop {
	case provider.StopEndTurn:
	case provider.StopMaxTokens:
		return "", ErrSummaryCutOff
	case provider.StopRefusal:
		return "", ErrSummaryRefused
	default:
		return "", fmt.Errorf("compact: the summary response stopped for the reason %q", stop)
	}
	summary := strings.Join(texts, "")
	if strings.TrimSpace(summary) == "" {
		return "", ErrEmptySummary
	}
	return summary, nil
}
