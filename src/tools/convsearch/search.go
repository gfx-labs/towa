package convsearch

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
	"uuid"

	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/transcript"
)

// Field says which kind of entry a piece of text is from.
type Field string

// Fields of an entry.
const (
	FieldUser       Field = "user"
	FieldText       Field = "text"
	FieldThinking   Field = "thinking"
	FieldToolCall   Field = "tool_call"
	FieldToolResult Field = "tool_result"
	// FieldCompaction is a compaction's summary. A range returns it. A
	// search does not look in it: it restates entries that are searched.
	FieldCompaction Field = "compaction"
	// FieldOther is an entry with no text: a usage record, a model switch,
	// an interruption mark or a provider's native item.
	FieldOther Field = "other"
)

// SearchInput is a keyword search.
type SearchInput struct {
	Target
	// Query is matched as one piece of text, spaces included: several
	// words match only where they stand together in that order. Letter
	// case is ignored.
	Query string
}

// Hit is one entry that holds the query.
type Hit struct {
	Session uuid.UUID
	// Position is the entry's place in its session's full history, counted
	// from zero. A range read with it returns the entry.
	Position int
	ID       uuid.UUID
	Time     time.Time
	Field    Field
	// Tool is the tool's name, for a tool call or a tool result.
	Tool string
	// Snippet holds the first match and Limits.SnippetContextBytes on
	// each side. CutBefore and CutAfter say the entry's text
	// goes on past that end of it.
	Snippet   string
	CutBefore bool
	CutAfter  bool
	// Stored is the hash of the stored blob the match is in. It is empty
	// when the match is in the entry itself.
	Stored string
}

// SearchResult is what a search found.
type SearchResult struct {
	// Hits are in the order the sessions were read, the calling session
	// first, and within a session from the oldest entry to the newest.
	Hits []Hit
	// Omitted is how many more entries hold the query than Limits.Hits let
	// through.
	Omitted int
	// Sessions and Entries are how much was read.
	Sessions int
	Entries  int
	// StoredSearched and StoredSkipped count the stored blobs that were
	// read and the ones that were not, because they are larger than
	// Limits.BlobBytes or are not text.
	StoredSearched int
	StoredSkipped  int
}

// Search finds the entries that hold a query, in every session the target
// covers.
func Search(ctx context.Context, st store.Store, access Access, limits Limits, in SearchInput) (SearchResult, error) {
	if err := limits.check(); err != nil {
		return SearchResult{}, err
	}
	if in.Query == "" {
		return SearchResult{}, &Error{Kind: KindEmptyQuery}
	}
	metas, err := sessions(ctx, st, access, in.Target)
	if err != nil {
		return SearchResult{}, err
	}
	s := searcher{st: st, limits: limits, query: strings.ToLower(in.Query)}
	for _, meta := range metas {
		entries, err := st.History(ctx, meta.ID, store.Range{})
		if err != nil {
			return SearchResult{}, storeError(meta.ID, err)
		}
		s.result.Sessions++
		position := 0
		for entry, err := range entries {
			if err != nil {
				return SearchResult{}, storeError(meta.ID, err)
			}
			if err := s.entry(ctx, meta.ID, position, entry); err != nil {
				return SearchResult{}, err
			}
			position++
		}
	}
	return s.result, nil
}

type searcher struct {
	st     store.Store
	limits Limits
	// query is lowercased.
	query  string
	result SearchResult
}

// piece is one stretch of an entry's text. The query is matched inside a
// piece and never across two.
type piece struct {
	text string
	// stored is the hash of the blob the text was read from.
	stored string
}

func (s *searcher) entry(ctx context.Context, session uuid.UUID, position int, entry transcript.Entry) error {
	s.result.Entries++
	field, tool, texts, stored := content(entry.Item)
	if field == FieldCompaction || field == FieldOther {
		return nil
	}
	pieces := make([]piece, 0, len(texts)+len(stored))
	for _, text := range texts {
		pieces = append(pieces, piece{text: text})
	}
	for _, ref := range stored {
		text, ok, err := s.blob(ctx, session, ref)
		if err != nil {
			return err
		}
		if !ok {
			s.result.StoredSkipped++
			continue
		}
		s.result.StoredSearched++
		pieces = append(pieces, piece{text: text, stored: ref.Hash})
	}
	for _, p := range pieces {
		lower := strings.ToLower(p.text)
		at := strings.Index(lower, s.query)
		if at < 0 {
			continue
		}
		if len(s.result.Hits) >= s.limits.Hits {
			s.result.Omitted++
			return nil
		}
		start, end := originalRange(p.text, at, at+len(s.query))
		from, to := window(p.text, start, end, s.limits.SnippetContextBytes)
		s.result.Hits = append(s.result.Hits, Hit{
			Session: session, Position: position, ID: entry.ID, Time: entry.Time,
			Field: field, Tool: tool,
			Snippet: p.text[from:to], CutBefore: from > 0, CutAfter: to < len(p.text),
			Stored: p.stored,
		})
		return nil
	}
	return nil
}

// blob reads a stored blob as text. It reports false for one that is over
// the limit or is not UTF-8 text.
func (s *searcher) blob(ctx context.Context, session uuid.UUID, ref *transcript.Stored) (string, bool, error) {
	if s.limits.BlobBytes == 0 || ref.Size > s.limits.BlobBytes {
		return "", false, nil
	}
	rc, err := s.st.OpenBlob(ctx, session, ref.Hash)
	if err != nil {
		return "", false, fmt.Errorf("convsearch: session %s: blob %s: %w", session, ref.Hash, err)
	}
	// The limit is applied to the read as well: the size in the reference
	// is what the entry says, not what the store holds.
	data, err := io.ReadAll(io.LimitReader(rc, s.limits.BlobBytes+1))
	if err = errors.Join(err, rc.Close()); err != nil {
		return "", false, fmt.Errorf("convsearch: session %s: blob %s: %w", session, ref.Hash, err)
	}
	if int64(len(data)) > s.limits.BlobBytes || !utf8.Valid(data) {
		return "", false, nil
	}
	return string(data), true, nil
}

// content returns an item's field, its tool name, its text in pieces and
// the stored blobs it references.
func content(item transcript.Item) (Field, string, []string, []*transcript.Stored) {
	switch {
	case item.User != nil:
		texts, stored := parts(item.User.Parts)
		return FieldUser, "", texts, stored
	case item.Text != nil:
		return FieldText, "", []string{item.Text.Text}, nil
	case item.Thinking != nil:
		return FieldThinking, "", []string{item.Thinking.Text}, nil
	case item.ToolCall != nil:
		return FieldToolCall, item.ToolCall.Name, []string{item.ToolCall.Name, callInput(item.ToolCall)}, nil
	case item.ToolResult != nil:
		texts, stored := parts(item.ToolResult.Parts)
		return FieldToolResult, item.ToolResult.Name, texts, stored
	case item.Compaction != nil:
		return FieldCompaction, "", []string{item.Compaction.Summary}, nil
	default:
		return FieldOther, "", nil, nil
	}
}

// callInput is a tool call's argument as text: the text itself for a tool
// that takes free text, and the JSON as the model wrote it otherwise. A
// quote or a backslash inside a JSON argument is therefore searched in its
// escaped form.
func callInput(call *transcript.ToolCall) string {
	if call.Input.Kind() == '"' {
		var text string
		if err := json.Unmarshal(call.Input, &text); err == nil {
			return text
		}
	}
	return string(call.Input)
}

func parts(ps []transcript.Part) ([]string, []*transcript.Stored) {
	var texts []string
	var stored []*transcript.Stored
	for _, p := range ps {
		switch {
		case p.Stored != nil:
			stored = append(stored, p.Stored)
		case p.Text != "":
			texts = append(texts, p.Text)
		case p.Resource != nil:
			texts = append(texts, p.Resource.URI)
			if p.Resource.Text != "" {
				texts = append(texts, p.Resource.Text)
			}
		case p.Link != nil:
			texts = append(texts, p.Link.Name, p.Link.URI)
		}
	}
	return texts, stored
}

// originalRange maps the byte range lo to hi of strings.ToLower(text) back
// to the bytes of text that cover the same characters. Lowercasing changes
// a character's length for some letters, so an offset into the lowered copy
// is not an offset into the text.
func originalRange(text string, lo, hi int) (int, int) {
	lowered, start := 0, -1
	for i, r := range text {
		if lowered >= hi {
			if start < 0 {
				start = i
			}
			return start, i
		}
		// strings.ToLower writes unicode.ToLower of each rune, and the
		// replacement character for a byte that is not UTF-8.
		n := utf8.RuneLen(unicode.ToLower(r))
		if start < 0 && lowered+n > lo {
			start = i
		}
		lowered += n
	}
	if start < 0 {
		start = len(text)
	}
	return start, len(text)
}

// window keeps the whole match, with context bytes on each side rounded
// inward to whole characters.
func window(text string, start, end, limit int) (int, int) {
	from := start - min(start, limit)
	to := end + min(len(text)-end, limit)
	for from < start && !utf8.RuneStart(text[from]) {
		from++
	}
	for to > from && to < len(text) && !utf8.RuneStart(text[to]) {
		to--
	}
	return from, to
}
