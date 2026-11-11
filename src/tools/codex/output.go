package codex

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// codex-rs/utils/string/src/truncate.rs:6 and core/src/unified_exec/mod.rs:74.
const (
	bytesPerToken int64 = 4
	defaultTokens int64 = 10000
)

func tokenCount(bytes int64) int64 { return (bytes + bytesPerToken - 1) / bytesPerToken }

func truncate(text string, tokens int64) string {
	budget := tokens * bytesPerToken
	if int64(len(text)) <= budget {
		return text
	}
	left := int(budget / 2)
	right := len(text) - int(budget-budget/2)
	for left > 0 && !utf8.RuneStart(text[left]) {
		left--
	}
	for right < len(text) && !utf8.RuneStart(text[right]) {
		right++
	}
	return text[:left] + fmt.Sprintf("…%d tokens truncated…", tokenCount(int64(len(text))-budget)) + text[right:]
}

func lines(text string) int {
	if text == "" {
		return 0
	}
	n := strings.Count(text, "\n")
	if !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// core/src/tools/context.rs:495-574. Chunk ID is optional and is omitted here.
func execText(text string, duration time.Duration, code int, tokens, omitted int64) string {
	originalBytes := int64(len(text))
	if omitted > 0 {
		marker := fmt.Sprintf("\n... %d bytes omitted ...\n", omitted)
		originalBytes += omitted - int64(len(marker))
	}
	original := tokenCount(originalBytes)
	header := fmt.Sprintf("Wall time: %.4f seconds\nProcess exited with code %d\nOriginal token count: %d\nOutput:", duration.Seconds(), code, original)
	render := func(budget int64) string {
		if int64(len(text)) <= budget*bytesPerToken {
			return text
		}
		result := truncate(text, budget)
		if omitted > 0 {
			marker := fmt.Sprintf("... %d bytes omitted ...", omitted)
			notice := ""
			if !strings.Contains(result, marker) {
				notice = marker + "\n"
			}
			return fmt.Sprintf("Warning: truncated output (original token count: %d)\n%s\n%s", original, notice, result)
		}
		return fmt.Sprintf("Warning: truncated output (original token count: %d)\nTotal output lines: %d\n\n%s", original, lines(text), result)
	}
	// Upstream adds a 20 percent serialization allowance to the model budget.
	outputBudget := defaultTokens*6/5*bytesPerToken - int64(len(header)) - 1
	out := render(tokens)
	for int64(len(out)) > outputBudget && tokens > 0 {
		tokens = max(0, tokens-tokenCount(int64(len(out))-outputBudget))
		out = render(tokens)
	}
	return header + "\n" + out
}
