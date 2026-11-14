package core

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/fsops"
	"github.com/gfx-labs/towa/src/tools/shell"
	"github.com/gfx-labs/towa/src/transcript"
)

// bashOutputCharacters is the most command output the model is given, in
// UTF-16 units. It is Claude Code 2.1.280's default BASH_MAX_OUTPUT_LENGTH.
const bashOutputCharacters = 30000

func readResult(read fsops.ReadResult) tools.Result {
	if read.Image != nil {
		return tools.Result{Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Image: read.Image}}}
	}
	if read.Text == "" {
		return tools.TextResult(transcript.OutcomeCompleted, "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>")
	}
	// The lines go as they are in the file, without their numbers: a number
	// and a tab on every line were about a tenth of a source file's tokens.
	// The capability keeps line endings, and a CRLF's CR is removed here.
	text := strings.TrimSuffix(strings.ReplaceAll(strings.TrimSuffix(read.Text, "\n"), "\r\n", "\n"), "\r")
	if strings.TrimSpace(text) == "" {
		// Blank lines alone would read as a result that was lost.
		text = fmt.Sprintf("[Lines %d to %d hold only whitespace.]", read.FirstLine, read.FirstLine+read.Lines-1)
	}
	if read.More {
		text += fmt.Sprintf("\n[More lines remain. Continue with offset %d.]", read.FirstLine+read.Lines)
	}
	return tools.TextResult(transcript.OutcomeCompleted, text)
}

func bashResult(run shell.Result) tools.Result {
	output := truncateBash(run.Output)
	if run.DroppedBytes > 0 {
		// The capability keeps a tail too, but only the start is shown. Say
		// so when the configured byte limit came first.
		output += fmt.Sprintf("\n[Configured output byte limit omitted %d bytes; retained tail also not shown.]", run.DroppedBytes)
	}
	// Leading blank lines and trailing space are not shown.
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
	result := tools.TextResult(transcript.OutcomeCompleted, output)
	result.Outcome = run.Outcome()
	return result
}

func truncateBash(output string) string {
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
