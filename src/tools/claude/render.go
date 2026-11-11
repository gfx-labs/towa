package claude

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/tools/fsops"
	"github.com/gfx-labs/acpharness/src/tools/shell"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// 2.1.280 chunk-s5969tjt.js:11, default BASH_MAX_OUTPUT_LENGTH.
const bashOutputCharacters = 30000

func readResult(read fsops.ReadResult) tools.Result {
	if read.Image != nil {
		return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Image: read.Image}}}
	}
	if read.Text == "" {
		return textResult("<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>", false)
	}
	var text strings.Builder
	// Upstream bNn uses line number + tab with its tab-separator flag off.
	// The capability preserves line endings; the renderer removes CRLF's CR.
	lines := strings.Split(strings.TrimSuffix(read.Text, "\n"), "\n")
	for i, line := range lines {
		if i > 0 {
			text.WriteByte('\n')
		}
		fmt.Fprintf(&text, "%d\t%s", read.FirstLine+i, strings.TrimSuffix(line, "\r"))
	}
	if read.More {
		// This marker is harness-authored; total line counts are not returned
		// by fsops.Read, so do not invent an upstream continuation summary.
		fmt.Fprintf(&text, "\n[More lines remain. Continue with offset %d.]", read.FirstLine+read.Lines)
	}
	return textResult(text.String(), false)
}

func bashResult(run shell.Result) tools.Result {
	output := truncateBash(run.Output)
	if run.DroppedBytes > 0 {
		// The capability keeps a tail too, but upstream's character cap keeps
		// only a prefix. Be explicit when the configured byte cap came first.
		output += fmt.Sprintf("\n[Configured output byte limit omitted %d bytes; retained tail also not shown.]", run.DroppedBytes)
	}
	// The inspected Bash map strips leading blank lines and trailing space.
	for {
		line, rest, found := strings.Cut(output, "\n")
		if !found || strings.TrimSpace(line) != "" {
			break
		}
		output = rest
	}
	output = strings.TrimRightFunc(output, unicode.IsSpace)
	var suffix string
	switch run.End {
	case shell.EndCancelled:
		suffix = "<error>Command was aborted before completion</error>"
	case shell.EndTimedOut:
		// Upstream may auto-background instead. This harness has no background
		// session, so a timeout stops the process and says so explicitly.
		suffix = fmt.Sprintf("<error>Command timed out after %d milliseconds and was stopped</error>", run.Timeout.Milliseconds())
	default:
		code := run.ExitCode
		if run.SignalNumber != 0 {
			code = 128 + run.SignalNumber
		}
		if code != 0 {
			suffix = fmt.Sprintf("Exit code %d", code)
		}
	}
	if suffix != "" {
		if output != "" {
			output += "\n"
		}
		output += suffix
	}
	if run.PipeHeld {
		output += "\n[Output pipe remained open after the configured drain deadline.]"
	}
	result := textResult(output, false)
	result.Outcome = run.Outcome()
	return result
}

func truncateBash(output string) string {
	// JavaScript's string length counts UTF-16 units, not UTF-8 bytes.
	units := 0
	for offset, r := range output {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > bashOutputCharacters {
			// Keep valid UTF-8 rather than half of a surrogate pair at a cut.
			return fmt.Sprintf("%s\n\n... [%d lines truncated] ...", output[:offset], strings.Count(output[offset:], "\n")+1)
		}
		units += width
	}
	return output
}
