package compact

import (
	"errors"
	"fmt"
	"strings"

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
