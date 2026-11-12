package models

import (
	"slices"
	"strconv"
	"strings"
)

// name is a model id taken apart: "claude-opus-5-5" is vendor claude, family
// opus, version 5.5, and "gpt-6.1-sol" is vendor gpt, family sol, version 6.1.
type name struct {
	vendor, family string
	version        [2]int
}

// dateDigits is the length of the release date some ids end in
// ("claude-opus-5-5-20261001").
const dateDigits = 8

// parseName takes a model id apart. It reports false for an id without a
// version, and for a gateway's id ("anthropic/claude-opus-4.7"): the window
// that counts there is the gateway's.
func parseName(id string) (name, bool) {
	if strings.Contains(id, "/") {
		return name{}, false
	}
	segments := strings.Split(strings.ToLower(id), "-")
	n := name{vendor: segments[0]}
	segments = segments[1:]
	if last := len(segments) - 1; last >= 0 && len(segments[last]) == dateDigits && isNumber(segments[last]) {
		segments = segments[:last]
	}
	var numbers []int
	for _, segment := range segments {
		parts := strings.Split(segment, ".")
		if !slices.ContainsFunc(parts, func(p string) bool { return !isNumber(p) }) {
			for _, part := range parts {
				number, err := strconv.Atoi(part)
				if err != nil {
					return name{}, false
				}
				numbers = append(numbers, number)
			}
			continue
		}
		if n.family == "" {
			n.family = segment
		}
	}
	if n.vendor == "" || len(numbers) == 0 {
		return name{}, false
	}
	copy(n.version[:], numbers)
	return n, true
}

func isNumber(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

func newer(a, b [2]int) bool {
	return a[0] > b[0] || (a[0] == b[0] && a[1] > b[1])
}

// guess returns the figures of a model the table does not have, taken from
// the row its name points to, and that row's id. A new model then works on
// the day its name is configured, without a row of its own.
//
//   - The same model under another spelling or with a release date takes
//     its own row.
//   - A newer version of a family in the table takes the family's newest row.
//   - A family the table does not have takes the vendor's newest rows, when
//     its version is not older than theirs and they agree.
//
// An older model that has no row is not guessed: nothing says an old model
// has the figures of a new one.
func guess(id string) (Metadata, string, bool) {
	want, ok := parseName(id)
	if !ok {
		return Metadata{}, "", false
	}
	var sameFamily, sameVendor []string
	for row := range builtin {
		have, ok := parseName(row)
		if !ok || have.vendor != want.vendor {
			continue
		}
		if have == want {
			return builtin[row], row, true
		}
		sameVendor = append(sameVendor, row)
		if have.family == want.family {
			sameFamily = append(sameFamily, row)
		}
	}
	rows, atLeast := sameFamily, false
	if len(rows) == 0 {
		rows, atLeast = sameVendor, true
	}
	return newest(rows, want.version, atLeast)
}

// newest returns the figures of the rows with the highest version, when
// version is newer than theirs (or as new, with atLeast) and they agree.
func newest(rows []string, version [2]int, atLeast bool) (Metadata, string, bool) {
	var top []string
	var topVersion [2]int
	for _, row := range rows {
		have, _ := parseName(row)
		switch {
		case len(top) == 0 || newer(have.version, topVersion):
			top, topVersion = []string{row}, have.version
		case have.version == topVersion:
			top = append(top, row)
		}
	}
	if len(top) == 0 || newer(topVersion, version) || (!atLeast && topVersion == version) {
		return Metadata{}, "", false
	}
	slices.Sort(top)
	for _, row := range top[1:] {
		if builtin[row] != builtin[top[0]] {
			return Metadata{}, "", false
		}
	}
	return builtin[top[0]], top[0], true
}
