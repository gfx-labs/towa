package compact

import (
	"errors"
	"fmt"
	"unicode/utf8"
	"uuid"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
)

// jcode origin/master 8e11eb23e: crates/jcode-compaction-core/src/lib.rs:43
// and crates/jcode-base/src/compaction.rs:1792. These are estimates, not a
// tokenizer or a promise that the endpoint will accept the request.
const (
	summaryCharsPerToken = 4
	summaryReserveTokens = 4000
)

// Omission reports text left out of the summary input. Entry is nil for a
// context-budget cut across the serialized conversation. Bytes counts UTF-8
// bytes, not tokens. Native content has no readable text to count.
type Omission struct {
	Entry  uuid.UUID
	Reason string
	Bytes  int
}

// SummaryReport accompanies a bounded summary request. InputTokens is an
// estimate using jcode's four bytes per token. ReservedTokens includes the
// requested output allowance. The exact tokenizer and wire overhead belong
// to the provider, so a context-length refusal still leaves history intact.
type SummaryReport struct {
	InputTokens    int64
	ReservedTokens int64
	Omissions      []Omission
}

// BoundedSummaryRequest fits the serialized text into the summary model's
// context estimate. It retains the instructions and previous summary whole,
// removes oldest conversation text first, and reports every text cut. A
// fixed prompt that cannot fit is an error, not an empty summary request.
func BoundedSummaryRequest(in SummaryInput, contextWindow int64) (provider.Request, SummaryReport, error) {
	report := SummaryReport{ReservedTokens: max(summaryReserveTokens, in.MaxOutputTokens)}
	if in.MaxOutputTokens <= 0 || contextWindow <= report.ReservedTokens {
		return provider.Request{}, report, errors.New("compact: summary context must exceed its positive output allowance and reserve")
	}
	req, err := SummaryRequest(in)
	if err != nil {
		return provider.Request{}, report, err
	}
	var conversation string
	var wrap func(SummaryInput, string) string
	if in.Prompt == config.SummaryCheckpoint {
		conversation, wrap = checkpointConversation(in.Replaced), checkpointWrap
	} else {
		conversation, wrap = shortConversation(in.Replaced), shortWrap
	}
	report.Omissions = serializationOmissions(in)
	system := req.System[0].Text
	// Count all fixed text, including the previous summary and search sentence.
	fixed := len(system) + len(wrap(in, ""))
	available := contextWindow - report.ReservedTokens
	if int64(fixed)/summaryCharsPerToken >= available {
		return provider.Request{}, report, errors.New("compact: summary instructions and previous summary exceed the context budget")
	}
	text := conversation
	if estimatedSummaryTokens(system, wrap(in, text)) > available {
		var omitted int
		text, omitted, err = newestText(conversation, available, fixed)
		if err != nil {
			return provider.Request{}, report, err
		}
		report.Omissions = append(report.Omissions, Omission{Reason: "context budget", Bytes: omitted})
	}
	req.Items[0].User.Parts[0].Text = wrap(in, text)
	report.InputTokens = estimatedSummaryTokens(system, req.Items[0].User.Parts[0].Text)
	if report.InputTokens > available {
		return provider.Request{}, report, fmt.Errorf("compact: summary estimate %d exceeds budget %d", report.InputTokens, available)
	}
	return req, report, nil
}

// newestText drops the oldest bytes of conversation so that it fits in
// available tokens beside fixed bytes of other text, and puts a marker in
// their place. It returns the new text and the number of bytes dropped.
func newestText(conversation string, available int64, fixed int) (text string, omitted int, err error) {
	const marker = "[Oldest conversation text omitted to fit the summary context.]\n"
	// Work in bytes as jcode's estimate does, then advance to a UTF-8
	// boundary. The marker is part of the budget too.
	room := available*summaryCharsPerToken - int64(fixed+len(marker))
	if room <= 0 {
		return "", 0, errors.New("compact: no conversation text fits beside the summary instructions")
	}
	start := len(conversation) - int(min(room, int64(len(conversation))))
	for start < len(conversation) && !utf8.RuneStart(conversation[start]) {
		start++
	}
	return marker + conversation[start:], start, nil
}

func estimatedSummaryTokens(system, text string) int64 {
	return (int64(len(system)) + int64(len(text)) + summaryCharsPerToken - 1) / summaryCharsPerToken
}

// serializationOmissions lists what the summary text leaves out of the
// replaced entries: thinking, native content and the end of long tool
// results.
func serializationOmissions(in SummaryInput) []Omission {
	var out []Omission
	for _, e := range in.Replaced {
		item := e.Item
		switch {
		case item.Thinking != nil:
			out = append(out, Omission{Entry: e.ID, Reason: "thinking excluded", Bytes: len(item.Thinking.Text)})
		case item.Native != nil:
			out = append(out, Omission{Entry: e.ID, Reason: "unreadable native content"})
		case item.ToolResult != nil:
			text := partsText(item.ToolResult.Parts, "")
			if cut := len(text) - len(retainedResult(text, in.Prompt)); cut > 0 {
				out = append(out, Omission{Entry: e.ID, Reason: "tool result limit", Bytes: cut})
			}
		}
	}
	return out
}

// retainedResult is the part of a tool result's text that the summary
// request shows.
func retainedResult(text, prompt string) string {
	if prompt == config.SummaryShort {
		return cutBytes(text, shortResultBytes)
	}
	// The rendered marker is not retained source text.
	units, end := 0, 0
	for at, r := range text {
		n := 1
		if r > 0xffff {
			n = 2
		}
		if units+n > checkpointResultChars {
			break
		}
		units += n
		end = at + utf8.RuneLen(r)
	}
	return text[:end]
}
