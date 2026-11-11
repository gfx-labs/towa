package convsearch

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
)

// TurnsInput is a range of one session's entries.
type TurnsInput struct {
	// Target picks the session: Target.Session, or the calling session
	// when that is not set.
	Target
	// Start and End are positions in the session's full history, counted
	// from zero. End is exclusive. They are the positions a Hit carries.
	Start, End int
}

// Turn is one entry of a range.
type Turn struct {
	Position int
	ID       uuid.UUID
	Time     time.Time
	Field    Field
	// Tool is the tool's name, for a tool call or a tool result.
	Tool string
	// Text is the entry's text, its pieces joined by a line break, at most
	// Limits.HitBytes long. Cut says the text goes on, and Bytes is its
	// whole length.
	Text  string
	Cut   bool
	Bytes int
	// Stored counts the entry's parts whose content is a stored blob. A
	// range does not read them.
	Stored int
}

// TurnsResult is the entries of a range.
type TurnsResult struct {
	Session uuid.UUID
	Turns   []Turn
	// Omitted is how many more entries the range holds than Limits.Hits
	// let through. The ones returned are the first of the range.
	Omitted int
}

// Turns returns a range of a session's entries, including the ones from
// before a compaction. A range past the end of the history returns no
// entries.
func Turns(ctx context.Context, st store.Store, access Access, limits Limits, in TurnsInput) (TurnsResult, error) {
	if err := limits.check(); err != nil {
		return TurnsResult{}, err
	}
	if in.Start < 0 || in.End <= in.Start {
		return TurnsResult{}, &Error{Kind: KindInvalidRange, Start: in.Start, End: in.End}
	}
	target := in.Target
	if target.Session == (uuid.UUID{}) {
		target.Session = access.Session
	}
	metas, err := sessions(ctx, st, access, target)
	if err != nil {
		return TurnsResult{}, err
	}
	session := metas[0].ID
	entries, err := st.History(ctx, session, store.Range{From: in.Start, To: in.End})
	if err != nil {
		return TurnsResult{}, storeError(session, err)
	}
	result := TurnsResult{Session: session}
	position := in.Start
	for entry, err := range entries {
		if err != nil {
			return TurnsResult{}, storeError(session, err)
		}
		if len(result.Turns) >= limits.Hits {
			result.Omitted++
			continue
		}
		field, tool, texts, stored := content(entry.Item)
		text := strings.Join(texts, "\n")
		turn := Turn{
			Position: position, ID: entry.ID, Time: entry.Time, Field: field, Tool: tool,
			Text: text, Bytes: len(text), Stored: len(stored),
		}
		if len(text) > limits.HitBytes {
			to := limits.HitBytes
			for to > 0 && !utf8.RuneStart(text[to]) {
				to--
			}
			turn.Text, turn.Cut = text[:to], true
		}
		result.Turns = append(result.Turns, turn)
		position++
	}
	return result, nil
}
