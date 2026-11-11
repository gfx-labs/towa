package convsearch

import (
	"context"
	"time"
	"uuid"

	"github.com/gfx-labs/acpharness/src/store"
)

// StatsInput asks for totals.
type StatsInput struct {
	Target
}

// SessionStats are one session's totals over its full history.
type SessionStats struct {
	Session  uuid.UUID
	ParentID uuid.UUID
	Title    string
	// Entries is the number of entries, which is also one past the last
	// position a range can read.
	Entries int
	// Prompts is the number of user messages.
	Prompts int
	// ToolCalls is the number of tool calls.
	ToolCalls int
	// Compactions is the number of compaction entries. InContext is the
	// number of entries from the latest one on, which is what the model is
	// sent now. It equals Entries when there is no compaction.
	Compactions int
	InContext   int
	// Requests is the number of model requests that recorded their usage,
	// and the token counts are their sums. They are zero for a session
	// with no usage entries: nothing is estimated.
	Requests         int
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ThoughtTokens    int64
	// First and Last are the times of the oldest and the newest entry.
	// They are the zero time for a session with no entries.
	First, Last time.Time
}

// StatsResult is the totals of every session the target covers.
type StatsResult struct {
	// Sessions has the calling session first.
	Sessions []SessionStats
	// Total sums Sessions. Its Session, ParentID and Title are empty, and
	// its First and Last span all of them.
	Total SessionStats
}

// Stats returns the totals of every session the target covers.
func Stats(ctx context.Context, st store.Store, access Access, in StatsInput) (StatsResult, error) {
	metas, err := sessions(ctx, st, access, in.Target)
	if err != nil {
		return StatsResult{}, err
	}
	var result StatsResult
	for _, meta := range metas {
		entries, err := st.History(ctx, meta.ID, store.Range{})
		if err != nil {
			return StatsResult{}, storeError(meta.ID, err)
		}
		s := SessionStats{Session: meta.ID, ParentID: meta.ParentID, Title: meta.Title}
		lastCompaction := 0
		for entry, err := range entries {
			if err != nil {
				return StatsResult{}, storeError(meta.ID, err)
			}
			switch item := entry.Item; {
			case item.User != nil:
				s.Prompts++
			case item.ToolCall != nil:
				s.ToolCalls++
			case item.Compaction != nil:
				s.Compactions++
				lastCompaction = s.Entries
			case item.Usage != nil:
				s.Requests++
				s.InputTokens += item.Usage.InputTokens
				s.OutputTokens += item.Usage.OutputTokens
				s.CacheReadTokens += item.Usage.CacheReadTokens
				s.CacheWriteTokens += item.Usage.CacheWriteTokens
				s.ThoughtTokens += item.Usage.ThoughtTokens
			}
			s.span(entry.Time, entry.Time)
			s.Entries++
		}
		s.InContext = s.Entries - lastCompaction
		result.Sessions = append(result.Sessions, s)
		result.Total.add(s)
	}
	return result, nil
}

// span widens First and Last to hold a time range.
func (s *SessionStats) span(first, last time.Time) {
	if s.First.IsZero() || first.Before(s.First) {
		s.First = first
	}
	if last.After(s.Last) {
		s.Last = last
	}
}

func (s *SessionStats) add(o SessionStats) {
	s.Entries += o.Entries
	s.Prompts += o.Prompts
	s.ToolCalls += o.ToolCalls
	s.Compactions += o.Compactions
	s.InContext += o.InContext
	s.Requests += o.Requests
	s.InputTokens += o.InputTokens
	s.OutputTokens += o.OutputTokens
	s.CacheReadTokens += o.CacheReadTokens
	s.CacheWriteTokens += o.CacheWriteTokens
	s.ThoughtTokens += o.ThoughtTokens
	if o.Entries > 0 {
		s.span(o.First, o.Last)
	}
}
