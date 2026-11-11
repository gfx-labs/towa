package patch

// Ported from codex-rs/apply-patch/src/{seek_sequence,file_update,text_file}.rs
// at ce0323f2 (Apache-2.0).
import (
	"fmt"
	"sort"
	"strings"
)

func normalize(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u2010', '\u2011', '\u2012', '\u2013', '\u2014', '\u2015', '\u2212':
			return '-'
		case '\u2018', '\u2019', '\u201a', '\u201b':
			return '\''
		case '\u201c', '\u201d', '\u201e', '\u201f':
			return '"'
		case '\u00a0', '\u2002', '\u2003', '\u2004', '\u2005', '\u2006', '\u2007', '\u2008', '\u2009', '\u200a', '\u202f', '\u205f', '\u3000':
			return ' '
		default:
			return r
		}
	}, strings.TrimSpace(s))
}

func seek(lines, pattern []string, start int, atEOF bool) int {
	if len(pattern) == 0 {
		return start
	}
	if len(pattern) > len(lines) {
		return -1
	}
	if atEOF {
		start = max(start, len(lines)-len(pattern))
	}
	for _, transform := range []func(string) string{func(s string) string { return s }, trimEnd, strings.TrimSpace, normalize} {
		for i := start; i <= len(lines)-len(pattern); i++ {
			match := true
			for j, p := range pattern {
				if transform(lines[i+j]) != transform(p) {
					match = false
					break
				}
			}
			if match {
				return i
			}
		}
	}
	return -1
}

type (
	sourceLine  struct{ text, ending string }
	replacement struct {
		start, count int
		lines        []string
	}
)

func source(text string) ([]sourceLine, string) {
	var lines []sourceLine
	preferred := ""
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] != '\r' && text[i] != '\n' {
			continue
		}
		end := i + 1
		if text[i] == '\r' && end < len(text) && text[end] == '\n' {
			end++
		}
		ending := text[i:end]
		if preferred == "" {
			preferred = ending
		}
		lines = append(lines, sourceLine{text[start:i], ending})
		start = end
		i = end - 1
	}
	if start < len(text) {
		lines = append(lines, sourceLine{text[start:], ""})
	}
	if preferred == "" {
		preferred = "\n"
	}
	return lines, preferred
}

// Derive returns a file's new text, retaining the original context and line endings.
//
//nolint:staticcheck // These model-visible errors copy upstream capitalization.
func Derive(path, text string, chunks []Chunk) (string, error) {
	lines, ending := source(text)
	original := make([]string, len(lines))
	for i, l := range lines {
		original[i] = l.text
	}
	var edits []replacement
	cursor := 0
	for _, c := range chunks {
		if c.Context != nil {
			at := seek(original, []string{*c.Context}, cursor, false)
			if at < 0 {
				return "", fmt.Errorf("Failed to find context '%s' in %s", *c.Context, path)
			}
			cursor = at + 1
		}
		if len(c.Old) == 0 {
			edits = append(edits, replacement{len(lines), 0, c.New})
			continue
		}
		old, newLines := c.Old, c.New
		at := seek(original, old, cursor, c.EOF)
		if at < 0 && old[len(old)-1] == "" {
			old = old[:len(old)-1]
			if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
				newLines = newLines[:len(newLines)-1]
			}
			at = seek(original, old, cursor, c.EOF)
		}
		if at < 0 {
			return "", fmt.Errorf("Failed to find expected lines in %s:\n%s", path, strings.Join(c.Old, "\n"))
		}
		oldStart, newStart := 0, 0
		for _, pair := range c.ContextIndices {
			a, b := pair[0], pair[1]
			if a >= len(old) || b >= len(newLines) {
				break
			}
			if oldStart != a || newStart != b {
				edits = append(edits, replacement{at + oldStart, a - oldStart, newLines[newStart:b]})
			}
			oldStart, newStart = a+1, b+1
		}
		if oldStart != len(old) || newStart != len(newLines) {
			edits = append(edits, replacement{at + oldStart, len(old) - oldStart, newLines[newStart:]})
		}
		cursor = at + len(old)
	}
	sort.SliceStable(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var out strings.Builder
	index := 0
	writeOriginal := func(until int) {
		for ; index < until; index++ {
			out.WriteString(lines[index].text)
			e := lines[index].ending
			if e == "" {
				e = ending
			}
			out.WriteString(e)
		}
	}
	for _, edit := range edits {
		if edit.start < index || edit.start+edit.count > len(lines) {
			return "", fmt.Errorf("overlapping replacements in %s", path)
		}
		writeOriginal(edit.start)
		index += edit.count
		for _, line := range edit.lines {
			out.WriteString(line)
			out.WriteString(ending)
		}
	}
	writeOriginal(len(lines))
	return out.String(), nil
}
