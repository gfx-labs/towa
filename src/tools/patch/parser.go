// Package patch parses and applies the Codex apply_patch format.
// Ported from openai/codex ce0323f2, codex-rs/apply-patch/src/parser.rs
// and streaming_parser.rs (Apache-2.0).
package patch

import (
	"fmt"
	"strings"
	"unicode"
)

// Kind is the operation on one file.
type Kind string

// File operations.
const (
	Add    Kind = "add"
	Delete Kind = "delete"
	Update Kind = "update"
)

// Chunk replaces lines after an optional context marker.
type Chunk struct {
	Context        *string
	Old, New       []string
	ContextIndices [][2]int
	EOF            bool
}

// Hunk is one file operation, in patch order.
type Hunk struct {
	Kind                Kind
	Path, Move, Content string
	Chunks              []Chunk
}

// Patch is the parsed input. EnvironmentID is recognized but not selected here.
type Patch struct {
	Hunks         []Hunk
	EnvironmentID string
}

// ParseError is an invalid patch or hunk. Line is zero for patch boundaries.
type ParseError struct {
	Line    int
	Message string
}

func (e *ParseError) Error() string {
	if e.Line == 0 {
		return "invalid patch: " + e.Message
	}
	return fmt.Sprintf("invalid hunk at line %d, %s", e.Line, e.Message)
}

const (
	beginMarker       = "*** Begin Patch"
	endMarker         = "*** End Patch"
	eofMarker         = "*** End of File"
	environmentPrefix = "*** Environment ID:"
	movePrefix        = "*** Move to: "
	firstLineMessage  = "The first line of the patch must be '*** Begin Patch'"
	lastLineMessage   = "The last line of the patch must be '*** End Patch'"
	noLinesMessage    = "Update hunk does not contain any lines"
)

type mode int

const (
	started mode = iota
	adding
	deleting
	updating
	ended
)

var fileHeaders = []struct {
	prefix string
	kind   Kind
	mode   mode
}{
	{"*** Add File: ", Add, adding},
	{"*** Delete File: ", Delete, deleting},
	{"*** Update File: ", Update, updating},
}

func patchError(message string) error { return &ParseError{Message: message} }

func hunkError(line int, message string) error { return &ParseError{Line: line, Message: message} }

func unexpectedLine(line int, text string) error {
	return hunkError(line, fmt.Sprintf("Unexpected line found in update hunk: '%s'. Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)", text))
}

func missingMarker(line int, text string) error {
	return hunkError(line, fmt.Sprintf("Expected update hunk to start with a @@ context marker, got: '%s'", text))
}

func trimEnd(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

func (c *Chunk) empty() bool { return len(c.Old) == 0 && len(c.New) == 0 }

// Parse accepts the upstream lenient heredoc wrapper and preserves hunk order.
func Parse(text string) (Patch, error) {
	lines, err := patchLines(text)
	if err != nil {
		return Patch{}, err
	}
	p := parser{last: len(lines)}
	for i, raw := range lines[1:] {
		if err := p.line(i+2, raw); err != nil {
			return Patch{}, err
		}
	}
	if p.mode != ended {
		return Patch{}, patchError(lastLineMessage)
	}
	return p.patch, nil
}

func patchLines(text string) ([]string, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	err := checkBoundaries(lines)
	if err == nil {
		return lines, nil
	}
	if !heredoc(lines) {
		return nil, err
	}
	lines = lines[1 : len(lines)-1]
	if err := checkBoundaries(lines); err != nil {
		return nil, err
	}
	return lines, nil
}

func checkBoundaries(lines []string) error {
	if strings.TrimSpace(lines[0]) != beginMarker {
		return patchError(firstLineMessage)
	}
	if last := strings.TrimSpace(lines[len(lines)-1]); last != endMarker {
		return patchError(fmt.Sprintf("%s, and this patch ends with '%s'. Nothing was applied. Send the whole patch again with '%s' as its own last line.", lastLineMessage, last, endMarker))
	}
	return nil
}

func heredoc(lines []string) bool {
	if len(lines) < 4 || !strings.HasSuffix(lines[len(lines)-1], "EOF") {
		return false
	}
	switch lines[0] {
	case "<<EOF", "<<'EOF'", `<<"EOF"`:
		return true
	}
	return false
}

// parser follows codex-rs StreamingPatchParser over a complete patch.
type parser struct {
	patch    Patch
	mode     mode
	added    strings.Builder
	hunkLine int
	last     int
}

func (p *parser) line(number int, raw string) error {
	trimmed := strings.TrimSpace(raw)
	if p.mode == ended {
		if trimmed != "" {
			return patchError(lastLineMessage)
		}
		return nil
	}
	header := trimmed
	if p.mode == updating {
		header = trimEnd(raw)
	}
	// Upstream finish() accepts a whitespace-padded end marker on the final line.
	if number == p.last && trimmed == endMarker {
		header = endMarker
	}
	if p.mode == started && strings.HasPrefix(header, environmentPrefix) {
		return p.environment(header)
	}
	if matched, err := p.header(number, header); matched || err != nil {
		return err
	}
	switch p.mode {
	case adding:
		if added, ok := strings.CutPrefix(raw, "+"); ok {
			p.added.WriteString(added)
			p.added.WriteByte('\n')
			return nil
		}
	case updating:
		return p.updateLine(number, raw, header)
	}
	return hunkError(number, fmt.Sprintf("'%s' is not a valid hunk header. Valid hunk headers: '*** Add File: {path}', '*** Delete File: {path}', '*** Update File: {path}'", trimmed))
}

func (p *parser) environment(header string) error {
	if p.patch.EnvironmentID != "" {
		return patchError("apply_patch environment_id cannot be specified more than once")
	}
	p.patch.EnvironmentID = strings.TrimSpace(strings.TrimPrefix(header, environmentPrefix))
	if p.patch.EnvironmentID == "" {
		return patchError("apply_patch environment_id cannot be empty")
	}
	return nil
}

func (p *parser) header(number int, header string) (bool, error) {
	if header == endMarker {
		if err := p.closeHunk(number, header); err != nil {
			return true, err
		}
		p.mode = ended
		return true, nil
	}
	for _, h := range fileHeaders {
		path, ok := strings.CutPrefix(header, h.prefix)
		if !ok {
			continue
		}
		if err := p.closeHunk(number, header); err != nil {
			return true, err
		}
		p.patch.Hunks = append(p.patch.Hunks, Hunk{Kind: h.kind, Path: path})
		p.mode, p.hunkLine = h.mode, number
		return true, nil
	}
	return false, nil
}

func (p *parser) current() *Hunk { return &p.patch.Hunks[len(p.patch.Hunks)-1] }

func (p *parser) closeHunk(number int, text string) error {
	switch p.mode {
	case adding:
		p.current().Content = p.added.String()
		p.added.Reset()
	case updating:
		h := p.current()
		if len(h.Chunks) == 0 {
			return hunkError(p.hunkLine, fmt.Sprintf("Update file hunk for path '%s' is empty", h.Path))
		}
		if h.Chunks[len(h.Chunks)-1].empty() {
			if text == endMarker {
				return hunkError(number, noLinesMessage)
			}
			return unexpectedLine(number, text)
		}
	}
	return nil
}

func (p *parser) updateLine(number int, raw, header string) error {
	h := p.current()
	var last *Chunk
	if len(h.Chunks) > 0 {
		last = &h.Chunks[len(h.Chunks)-1]
	}
	marker := header == "@@" || strings.HasPrefix(header, "@@ ")
	if last != nil && last.EOF {
		if header == "" {
			return nil
		}
		if !marker {
			return missingMarker(number, raw)
		}
	}
	if last == nil && h.Move == "" {
		if move, ok := strings.CutPrefix(header, movePrefix); ok {
			h.Move = move
			return nil
		}
	}
	switch {
	case marker:
		if last != nil && last.empty() {
			return unexpectedLine(number, raw)
		}
		c := Chunk{}
		if context, ok := strings.CutPrefix(header, "@@ "); ok {
			c.Context = &context
		}
		h.Chunks = append(h.Chunks, c)
	case header == eofMarker:
		if last != nil {
			if last.empty() {
				return hunkError(number, noLinesMessage)
			}
			last.EOF = true
		}
	case raw == "" || strings.ContainsRune(" +-", rune(raw[0])):
		h.addLine(raw)
	case last != nil && !last.empty():
		return missingMarker(number, raw)
	default:
		return unexpectedLine(number, raw)
	}
	return nil
}

func (h *Hunk) addLine(raw string) {
	if len(h.Chunks) == 0 {
		h.Chunks = append(h.Chunks, Chunk{})
	}
	c := &h.Chunks[len(h.Chunks)-1]
	if raw == "" {
		raw = " "
	}
	switch text := raw[1:]; raw[0] {
	case ' ':
		c.ContextIndices = append(c.ContextIndices, [2]int{len(c.Old), len(c.New)})
		c.Old = append(c.Old, text)
		c.New = append(c.New, text)
	case '+':
		c.New = append(c.New, text)
	default:
		c.Old = append(c.Old, text)
	}
}
