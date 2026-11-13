package compact

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/src/transcript"
)

// EmergencySummary is the summary recorded when the context is cut without
// asking a model. It is the earlier summary, if any, and jcode's sentence
// saying how much was cut, with the names of the tools the cut entries
// used. contextTokens and budget are in tokens.
func EmergencySummary(previous string, cut []transcript.Entry, contextTokens, budget int64) string {
	var parts []string
	if strings.TrimSpace(previous) != "" {
		parts = append(parts, previous)
	}
	parts = append(parts, fmt.Sprintf(emergencyFormat, len(spans(cut)), contextTokens/1000, budget/1000))
	if names := toolNames(cut); len(names) > 0 {
		parts = append(parts, "Tools used: "+strings.Join(names, ", "))
	}
	return strings.Join(parts, "\n\n")
}

// toolNames returns the names of the tools called in entries, sorted.
func toolNames(entries []transcript.Entry) []string {
	var names []string
	for _, e := range entries {
		if c := e.Item.ToolCall; c != nil && !slices.Contains(names, c.Name) {
			names = append(names, c.Name)
		}
	}
	slices.Sort(names)
	return names
}

// emergencyResult is the Result of a cut made without a model call.
func emergencyResult(source Source, decision Decision) *Result {
	summary := EmergencySummary(source.Previous, source.Replaced, decision.ContextTokens, decision.Budget)
	return &Result{Source: source, item: transcript.Item{Compaction: &transcript.Compaction{Summary: summary, Mode: ModeEmergency}}}
}

// emergencyFormat is the sentence jcode records when it cuts the context
// without a model summary: build_emergency_summary_text,
// crates/jcode-compaction-core/src/lib.rs:538. The arguments are the number
// of messages cut, the context's tokens and the budget, the last two in
// thousands.
const emergencyFormat = "**[Emergency compaction]**: %d messages were dropped to recover from context overflow. " +
	"The conversation had ~%dk tokens which exceeded the %dk limit."
