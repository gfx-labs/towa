package extras

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"uuid"

	"github.com/valyala/bytebufferpool"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/convsearch"
	"github.com/gfx-labs/towa/src/transcript"
)

// ConversationSearch reads full stored history within the configured scope.
func ConversationSearch(cfg config.ConversationSearch, st store.Store) (tools.Tool, error) {
	if !cfg.Enabled {
		return tools.Tool{}, fmt.Errorf("conversation_search: %w", ErrDisabled)
	}
	if st == nil {
		return tools.Tool{}, errors.New("conversation_search: no store")
	}
	t := newTool(NameConversationSearch, "Search the stored history of this session, including what compaction has removed from your context. Give query to find a phrase, turns to read a range of entries, or stats for counts. Positions count stored entries from zero, including entries before compaction.", acp.ToolKindSearch, []byte(`{"type":"object","properties":{"intent":{"type":"string"},"query":{"type":"string","description":"Search phrase, ignoring letter case."},"turns":{"type":"object","required":["start","end"],"properties":{"start":{"type":"integer","minimum":0},"end":{"type":"integer","minimum":1}}},"stats":{"type":"boolean","description":"Return entry and token counts."},"scope":{"type":"string","enum":["self","descendants","store"],"description":"self is this session, descendants adds its subagents, store is every stored session. Configuration sets the widest scope allowed."},"session":{"type":"string","description":"One session UUID within the scope."}}}`))
	t.Summary = "Search the stored history of this session"
	t.Guidance = []string{"After a compaction, look up an exact error message, command or code snippet from earlier with " + NameConversationSearch + " before guessing at it."}
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Query *string `json:"query"`
			Turns *struct {
				Start int `json:"start"`
				End   int `json:"end"`
			} `json:"turns"`
			Stats   bool             `json:"stats"`
			Scope   convsearch.Scope `json:"scope"`
			Session string           `json:"session"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		session, err := uuid.Parse(string(call.Env.SessionID))
		if err != nil {
			return tools.Result{}, fmt.Errorf("conversation_search: calling session: %w", err)
		}
		target := convsearch.Target{Scope: in.Scope}
		if in.Session != "" {
			target.Session, err = uuid.Parse(in.Session)
			if err != nil {
				return tools.TextResult(transcript.OutcomeFailed, "The session must be a UUID."), nil
			}
		}
		access := convsearch.Access{Session: session, Widest: convsearch.Scope(cfg.Scope)}
		limits := convsearch.Limits{Hits: cfg.MaxHits, SnippetContextBytes: cfg.SnippetContextBytes, MessageBytes: cfg.MessageBytes, ToolResultBytes: cfg.ToolResultBytes, BlobBytes: cfg.MaxBlobBytes}
		out := bytebufferpool.Get()
		defer bytebufferpool.Put(out)
		if in.Stats {
			res, err := convsearch.Stats(ctx, st, access, convsearch.StatsInput{Target: target})
			if err != nil {
				return conversationError(err)
			}
			_, _ = out.WriteString("## Conversation Stats\n\n")
			for _, s := range res.Sessions {
				_, _ = fmt.Fprintf(out, "- Session %s (%s): %d entries, %d in context, %d compactions, %d tool calls\n", s.Session, s.Title, s.Entries, s.InContext, s.Compactions, s.ToolCalls)
			}
			s := res.Total
			_, _ = fmt.Fprintf(out, "- Total entries: %d\n- Active entries in context: %d\n- Recorded requests: %d\n- Recorded input tokens: %d\n- Recorded output tokens: %d\n- Recorded cache read tokens: %d\n- Recorded cache write tokens: %d\n- Recorded thought tokens: %d\n\n", s.Entries, s.InContext, s.Requests, s.InputTokens, s.OutputTokens, s.CacheReadTokens, s.CacheWriteTokens, s.ThoughtTokens)
		}
		if in.Query != nil {
			res, err := convsearch.Search(ctx, st, access, limits, convsearch.SearchInput{Target: target, Query: *in.Query})
			if err != nil {
				return conversationError(err)
			}
			if len(res.Hits) == 0 {
				_, _ = fmt.Fprintf(out, "## Search Results\n\nNo results found for '%s'\n", *in.Query)
			} else {
				_, _ = fmt.Fprintf(out, "## Search Results for '%s'\n\nFound %d matches:\n\n", *in.Query, len(res.Hits)+res.Omitted)
				for _, h := range res.Hits {
					_, _ = fmt.Fprintf(out, "**Entry %d (%s, session %s):**\n", h.Position, h.Field, h.Session)
					if h.CutBefore {
						_, _ = out.WriteString("...")
					}
					_, _ = out.WriteString(h.Snippet)
					if h.CutAfter {
						_, _ = out.WriteString("...")
					}
					_, _ = out.WriteString("\n\n")
				}
				if res.Omitted > 0 {
					_, _ = fmt.Fprintf(out, "... and %d more results\n", res.Omitted)
				}
			}
			if res.StoredSkipped > 0 {
				_, _ = fmt.Fprintf(out, "%d stored blobs skipped (non-text or over conversation_search.max_blob_bytes).\n", res.StoredSkipped)
			}
		}
		if in.Turns != nil {
			res, err := convsearch.Turns(ctx, st, access, limits, convsearch.TurnsInput{Target: target, Start: in.Turns.Start, End: in.Turns.End})
			if err != nil {
				return conversationError(err)
			}
			_, _ = fmt.Fprintf(out, "## Entries %d-%d (session %s)\n\n", in.Turns.Start, in.Turns.End, res.Session)
			if len(res.Turns) == 0 {
				_, _ = out.WriteString("No entries found in that range.\n")
			}
			for _, entry := range res.Turns {
				_, _ = fmt.Fprintf(out, "**Entry %d (%s):**\n%s", entry.Position, entry.Field, entry.Text)
				if entry.Cut {
					_, _ = fmt.Fprintf(out, "... (truncated, %d bytes total)", entry.Bytes)
				}
				if entry.Stored > 0 {
					_, _ = fmt.Fprintf(out, "\n[%d stored parts not read]", entry.Stored)
				}
				_, _ = out.WriteString("\n\n")
			}
			if res.Omitted > 0 {
				_, _ = fmt.Fprintf(out, "... and %d more entries\n", res.Omitted)
			}
		}
		if out.Len() == 0 {
			_, _ = out.WriteString("Please provide a 'query' to search, 'turns' range to retrieve, or 'stats': true to see conversation statistics.")
		}
		return tools.TextResult(transcript.OutcomeCompleted, out.String()), nil
	}
	return t, nil
}

func conversationError(err error) (tools.Result, error) {
	e, ok := errors.AsType[*convsearch.Error](err)
	if !ok {
		return tools.Result{}, err
	}
	text := e.Error()
	switch e.Kind {
	case convsearch.KindScopeNotAllowed:
		text = fmt.Sprintf("Scope %q is wider than the allowed %q.", e.Scope, e.Widest)
	case convsearch.KindUnknownScope:
		text = fmt.Sprintf("Unknown scope %q. Use self, descendants or store.", e.Scope)
	case convsearch.KindOutOfScope:
		text = fmt.Sprintf("Session %s is outside the requested scope %q.", e.Session, e.Scope)
	case convsearch.KindNotFound:
		text = fmt.Sprintf("Session %s was not found.", e.Session)
	case convsearch.KindEmptyQuery:
		text = "The search query must not be empty."
	case convsearch.KindInvalidRange:
		text = "The range start must be nonnegative and end must be greater than start."
	}
	return tools.TextResult(transcript.OutcomeFailed, text), nil
}
