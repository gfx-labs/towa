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
	begin = "*** Begin Patch"
	end   = "*** End Patch"
	eof   = "*** End of File"
)

func trimEnd(s string) string { return strings.TrimRightFunc(s, unicode.IsSpace) }

// Parse accepts the upstream lenient heredoc wrapper and preserves hunk order.
func Parse(text string) (Patch, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	boundary := func(lines []string) error {
		if strings.TrimSpace(lines[0]) != begin {
			return &ParseError{Message: "The first line of the patch must be '*** Begin Patch'"}
		}
		if strings.TrimSpace(lines[len(lines)-1]) != end {
			return &ParseError{Message: "The last line of the patch must be '*** End Patch'"}
		}
		return nil
	}
	if err := boundary(lines); err != nil {
		if len(lines) >= 4 && (lines[0] == "<<EOF" || lines[0] == "<<'EOF'" || lines[0] == "<<\"EOF\"") && strings.HasSuffix(lines[len(lines)-1], "EOF") {
			lines = lines[1 : len(lines)-1]
			if err := boundary(lines); err != nil {
				return Patch{}, err
			}
		} else {
			return Patch{}, err
		}
	}
	var p Patch
	state := Kind("")
	var added strings.Builder
	finishAdd := func() {
		if state == Add {
			p.Hunks[len(p.Hunks)-1].Content = added.String()
			added.Reset()
		}
	}
	hunkLine := 0
	ended := false
	invalid := func(line int, message string) error { return &ParseError{Line: line, Message: message} }
	unexpected := func(line int, text string) error {
		return invalid(line, fmt.Sprintf("Unexpected line found in update hunk: '%s'. Every line should start with ' ' (context line), '+' (added line), or '-' (removed line)", text))
	}
	ensure := func(line int, text string) error {
		if state != Update {
			return nil
		}
		h := &p.Hunks[len(p.Hunks)-1]
		if len(h.Chunks) == 0 {
			return invalid(hunkLine, fmt.Sprintf("Update file hunk for path '%s' is empty", h.Path))
		}
		c := h.Chunks[len(h.Chunks)-1]
		if len(c.Old) == 0 && len(c.New) == 0 {
			if text == end {
				return invalid(line, "Update hunk does not contain any lines")
			}
			return unexpected(line, text)
		}
		return nil
	}
	for i, raw := range lines[1:] {
		number := i + 2
		trimmed := strings.TrimSpace(raw)
		if ended {
			if trimmed != "" {
				return Patch{}, &ParseError{Message: "The last line of the patch must be '*** End Patch'"}
			}
			continue
		}
		header := trimmed
		if state == Update {
			header = trimEnd(raw)
		}
		// finish() treats a final, whitespace-padded end marker specially.
		if number == len(lines) && trimmed == end {
			header = end
		}
		if state == "" && strings.HasPrefix(header, "*** Environment ID:") {
			if p.EnvironmentID != "" {
				return Patch{}, &ParseError{Message: "apply_patch environment_id cannot be specified more than once"}
			}
			p.EnvironmentID = strings.TrimSpace(strings.TrimPrefix(header, "*** Environment ID:"))
			if p.EnvironmentID == "" {
				return Patch{}, &ParseError{Message: "apply_patch environment_id cannot be empty"}
			}
			continue
		}
		if header == end {
			if err := ensure(number, header); err != nil {
				return Patch{}, err
			}
			finishAdd()
			ended = true
			continue
		}
		matched := false
		for _, marker := range []struct {
			prefix string
			kind   Kind
		}{{"*** Add File: ", Add}, {"*** Delete File: ", Delete}, {"*** Update File: ", Update}} {
			if strings.HasPrefix(header, marker.prefix) {
				if err := ensure(number, header); err != nil {
					return Patch{}, err
				}
				finishAdd()
				p.Hunks = append(p.Hunks, Hunk{Kind: marker.kind, Path: strings.TrimPrefix(header, marker.prefix)})
				state, hunkLine, matched = marker.kind, number, true
				break
			}
		}
		if matched {
			continue
		}
		if state == Add && strings.HasPrefix(raw, "+") {
			added.WriteString(raw[1:])
			added.WriteByte('\n')
			continue
		}
		if state != Update {
			return Patch{}, invalid(number, fmt.Sprintf("'%s' is not a valid hunk header. Valid hunk headers: '*** Add File: {path}', '*** Delete File: {path}', '*** Update File: {path}'", trimmed))
		}
		h := &p.Hunks[len(p.Hunks)-1]
		marker := header == "@@" || strings.HasPrefix(header, "@@ ")
		if len(h.Chunks) > 0 && h.Chunks[len(h.Chunks)-1].EOF {
			if header == "" {
				continue
			}
			if !marker {
				return Patch{}, invalid(number, fmt.Sprintf("Expected update hunk to start with a @@ context marker, got: '%s'", raw))
			}
		}
		if len(h.Chunks) == 0 && h.Move == "" && strings.HasPrefix(header, "*** Move to: ") {
			h.Move = strings.TrimPrefix(header, "*** Move to: ")
			continue
		}
		if marker {
			if len(h.Chunks) > 0 {
				c := h.Chunks[len(h.Chunks)-1]
				if len(c.Old) == 0 && len(c.New) == 0 {
					return Patch{}, unexpected(number, raw)
				}
			}
			c := Chunk{}
			if strings.HasPrefix(header, "@@ ") {
				ctx := header[3:]
				c.Context = &ctx
			}
			h.Chunks = append(h.Chunks, c)
			continue
		}
		if header == eof {
			if len(h.Chunks) > 0 {
				c := &h.Chunks[len(h.Chunks)-1]
				if len(c.Old) == 0 && len(c.New) == 0 {
					return Patch{}, invalid(number, "Update hunk does not contain any lines")
				}
				c.EOF = true
			}
			continue
		}
		if raw == "" || raw[0] == ' ' || raw[0] == '+' || raw[0] == '-' {
			if len(h.Chunks) == 0 {
				h.Chunks = append(h.Chunks, Chunk{})
			}
			c := &h.Chunks[len(h.Chunks)-1]
			switch {
			case raw == "" || raw[0] == ' ':
				text := ""
				if raw != "" {
					text = raw[1:]
				}
				c.ContextIndices = append(c.ContextIndices, [2]int{len(c.Old), len(c.New)})
				c.Old = append(c.Old, text)
				c.New = append(c.New, text)
			case raw[0] == '+':
				c.New = append(c.New, raw[1:])
			default:
				c.Old = append(c.Old, raw[1:])
			}
			continue
		}
		if len(h.Chunks) > 0 {
			c := h.Chunks[len(h.Chunks)-1]
			if len(c.Old) > 0 || len(c.New) > 0 {
				return Patch{}, invalid(number, fmt.Sprintf("Expected update hunk to start with a @@ context marker, got: '%s'", raw))
			}
		}
		return Patch{}, unexpected(number, raw)
	}
	if !ended {
		return Patch{}, &ParseError{Message: "The last line of the patch must be '*** End Patch'"}
	}
	return p, nil
}
