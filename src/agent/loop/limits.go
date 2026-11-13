package loop

import (
	"time"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/pkg/acp/harness"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/transcript"
)

// Spent is what a turn has used so far.
type Spent struct {
	// ToolCalls is how many tool calls were dispatched.
	ToolCalls int64
	// Wall is how long the turn has run.
	Wall time.Duration
	// The token counts are sums over the turn's model requests.
	// InputTokens excludes the cache counts, as in transcript.Usage.
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ThoughtTokens    int64
	// Requests is how many model requests reported usage.
	Requests int
}

// Add counts one model request's usage.
func (s *Spent) Add(u transcript.Usage) {
	s.InputTokens += u.InputTokens
	s.OutputTokens += u.OutputTokens
	s.CacheReadTokens += u.CacheReadTokens
	s.CacheWriteTokens += u.CacheWriteTokens
	s.ThoughtTokens += u.ThoughtTokens
	s.Requests++
}

// Sent is every token sent to the model, cached or not. It is what
// limits.max_input_tokens bounds.
func (s Spent) Sent() int64 {
	return s.InputTokens + s.CacheReadTokens + s.CacheWriteTokens
}

// Exceeded reports the config key of a limit that leaves no room for
// another model request. A limit of zero is off. A turn stops when it has
// used a limit up, so the limit is the most a turn may use, not the first
// amount that is too much.
func Exceeded(l config.Limits, s Spent) (key string, exceeded bool) {
	switch {
	case l.MaxWall > 0 && s.Wall >= l.MaxWall:
		return config.KeyMaxWall, true
	case l.MaxInputTokens > 0 && s.Sent() >= l.MaxInputTokens:
		return config.KeyMaxInputTokens, true
	case l.MaxOutputTokens > 0 && s.OutputTokens >= l.MaxOutputTokens:
		return config.KeyMaxOutputTokens, true
	}
	return "", false
}

// ToolCallsLeft is how many more tool calls the turn may dispatch. It
// reports false for unlimited when limits.max_tool_calls is off.
func ToolCallsLeft(l config.Limits, s Spent) (left int64, limited bool) {
	if l.MaxToolCalls == 0 {
		return 0, false
	}
	return max(l.MaxToolCalls-s.ToolCalls, 0), true
}

// LimitResult is how a turn that reached the limit named by key ends.
func LimitResult(key string, s Spent) Result {
	return Result{
		StopReason: acp.StopReasonMaxTurnRequests,
		Usage:      s.Usage(),
		Stop:       &harness.Stop{Kind: harness.StopLimit, Limit: key},
	}
}

// Usage returns the totals as the prompt response reports them, or nil when
// no request reported usage.
func (s Spent) Usage() *acp.Usage {
	if s.Requests == 0 {
		return nil
	}
	u := &acp.Usage{
		InputTokens:  int(s.InputTokens),
		OutputTokens: int(s.OutputTokens),
		TotalTokens:  int(s.Sent() + s.OutputTokens),
	}
	// The three optional counts are left out when zero: a provider that
	// does not report one sends nothing for it.
	if s.CacheReadTokens != 0 {
		u.CachedReadTokens = new(int(s.CacheReadTokens))
	}
	if s.CacheWriteTokens != 0 {
		u.CachedWriteTokens = new(int(s.CacheWriteTokens))
	}
	if s.ThoughtTokens != 0 {
		u.ThoughtTokens = new(int(s.ThoughtTokens))
	}
	return u
}
