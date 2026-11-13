package compact

import (
	"fmt"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/transcript"
)

// Action is what to do about the context before the next request.
type Action int

// Actions of a Decision.
const (
	// None: send the next request as it is.
	None Action = iota
	// Background: start a compaction and keep going. The next request is
	// sent without waiting for it.
	Background
	// Emergency: the next request would not fit. Cut the context before
	// sending it, without waiting for a model summary.
	Emergency
	// Stop: the next request would not fit and nothing may or can be cut.
	// The turn ends with a context-full reason.
	Stop
)

// String returns the action as it appears in logs.
func (a Action) String() string {
	switch a {
	case None:
		return "none"
	case Background:
		return "background"
	case Emergency:
		return "emergency"
	case Stop:
		return "stop"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Trigger is the rule that produced a Decision.
type Trigger string

// Triggers of a Decision.
const (
	// TriggerFraction and TriggerTokens: reactive mode's two thresholds.
	TriggerFraction Trigger = "reactive_fraction"
	TriggerTokens   Trigger = "reactive_tokens"
	// TriggerProjection: the projected context reaches the proactive
	// threshold.
	TriggerProjection Trigger = "proactive_projection"
	// TriggerTopicShift: the conversation moved to another topic.
	TriggerTopicShift Trigger = "topic_shift"
	// TriggerNative: the explicit native threshold was reached.
	TriggerNative Trigger = "native_threshold"
	// TriggerFull: the context is at the emergency threshold.
	TriggerFull Trigger = "context_full"
)

// Input is what Decide looks at.
type Input struct {
	Config config.Compaction
	// ContextWindow is the model's context size in tokens, from model
	// metadata.
	ContextWindow int64
	// Usage is what the last model request reported. Its input side is the
	// size of the context that was sent.
	Usage transcript.Usage
	// Messages is how many messages the active conversation holds, from
	// Messages.
	Messages int
	// Running says a compaction is already under way.
	Running bool
	// History holds the context size after each of the latest turns,
	// oldest first, with the last request's included. The caller keeps at
	// most Config.HistoryWindow of them. Proactive and semantic modes read
	// it.
	History []int64
	// TurnsSinceCompaction counts the turns since the last compaction, or
	// since the session began.
	TurnsSinceCompaction int
	// Embeddings holds one vector per recent turn, oldest first, at most
	// Config.EmbeddingHistoryWindow of them. Only semantic mode reads it.
	// With fewer than four it decides as proactive mode does.
	Embeddings [][]float32
	// NativeAvailable says the provider about to be asked can compact a
	// conversation itself.
	NativeAvailable bool
}

// Decision is what Decide concluded.
type Decision struct {
	Action  Action
	Trigger Trigger
	// Native says the compaction is the provider's own: call a Native and
	// not the summary model.
	Native bool
	// Keep is how many of the latest messages the compaction leaves, for
	// Cut. Semantic mode may keep more: see SemanticKeep.
	Keep int
	// ContextTokens and Budget are the two numbers that were compared.
	ContextTokens, Budget int64
}

// ContextTokens is the size of the context a request sent, from the usage
// it reported: the input tokens with those read from and written to the
// cache. What the model wrote in answer is not counted, as in jcode, so the
// next request is larger by that much.
func ContextTokens(u transcript.Usage) int64 {
	return u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
}

// Budget is the token count the thresholds are fractions of: the model's
// context window, or max_context_tokens when that is set and smaller.
func Budget(cfg config.Compaction, contextWindow int64) int64 {
	if cfg.MaxContextTokens > 0 && cfg.MaxContextTokens < contextWindow {
		return cfg.MaxContextTokens
	}
	return contextWindow
}

// Decide says whether to compact before the next request. It follows
// jcode's CompactionManager (crates/jcode-base/src/compaction.rs at
// 8e11eb23e: ensure_context_fits, should_compact_with, anti_signals_block,
// should_compact_proactively, should_compact_semantic).
func Decide(in Input) (Decision, error) {
	cfg := in.Config
	if in.ContextWindow <= 0 {
		return Decision{}, fmt.Errorf("compact: the model's context window is %d tokens", in.ContextWindow)
	}
	tokens, budget := ContextTokens(in.Usage), Budget(cfg, in.ContextWindow)
	d := Decision{ContextTokens: tokens, Budget: budget}
	fraction := float64(tokens) / float64(budget)

	// A request that would not fit comes before everything else, whatever
	// the mode and even with a compaction under way: its summary would
	// arrive too late.
	if fraction >= cfg.EmergencyThreshold {
		d.Trigger = TriggerFull
		if cfg.Mode == config.CompactionOff || in.Messages <= cfg.EmergencyKeepRecentMessages {
			d.Action = Stop
			return d, nil
		}
		d.Action, d.Keep = Emergency, cfg.EmergencyKeepRecentMessages
		return d, nil
	}
	if in.Running || in.Messages <= cfg.KeepRecentMessages {
		return d, nil
	}
	d.Keep = cfg.KeepRecentMessages

	if in.NativeAvailable {
		switch cfg.OpenAINative {
		case config.NativeAuto:
			// The server compacts in the stream at its own threshold.
			return d, nil
		case config.NativeExplicit:
			if tokens >= cfg.OpenAINativeThresholdTokens {
				d.Action, d.Trigger, d.Native = Background, TriggerNative, true
			}
			return d, nil
		case config.NativeOff:
		default:
			return Decision{}, fmt.Errorf("compact: native mode %q is not known", cfg.OpenAINative)
		}
	}

	switch cfg.Mode {
	case config.CompactionOff:
	case config.CompactionReactive:
		switch {
		case fraction >= cfg.ReactiveThreshold:
			d.Action, d.Trigger = Background, TriggerFraction
		case cfg.ReactiveThresholdTokens > 0 && tokens >= cfg.ReactiveThresholdTokens:
			d.Action, d.Trigger = Background, TriggerTokens
		}
	case config.CompactionProactive:
		if !held(in, tokens, fraction) && projected(in, budget) {
			d.Action, d.Trigger = Background, TriggerProjection
		}
	case config.CompactionSemantic:
		switch {
		case held(in, tokens, fraction):
		case TopicShift(in.Embeddings, cfg.TopicShiftThreshold):
			d.Action, d.Trigger = Background, TriggerTopicShift
		case projected(in, budget):
			d.Action, d.Trigger = Background, TriggerProjection
		}
	default:
		return Decision{}, fmt.Errorf("compact: mode %q is not known", cfg.Mode)
	}
	return d, nil
}

// held reports a reason not to compact ahead of need: the context is below
// the floor, there is too little history, growth has stalled, or the last
// compaction was too recent.
func held(in Input, tokens int64, fraction float64) bool {
	cfg := in.Config
	floorReached := cfg.ProactiveFloorTokens > 0 && tokens >= cfg.ProactiveFloorTokens
	if fraction < cfg.ProactiveFloor && !floorReached {
		return true
	}
	if len(in.History) < cfg.MinSamples {
		return true
	}
	if n := len(in.History); n >= cfg.StallWindow && in.History[n-1] <= in.History[n-cfg.StallWindow] {
		return true
	}
	return in.TurnsSinceCompaction < cfg.MinTurnsBetween
}

// projected reports whether the context, growing as it has, reaches the
// proactive threshold within the lookahead. The growth per turn is an
// exponentially weighted average of the increases, and a decrease counts as
// no growth.
func projected(in Input, budget int64) bool {
	cfg, h := in.Config, in.History
	if len(h) < 2 {
		return false
	}
	growth := max(float64(h[1]-h[0]), 0)
	for i := 2; i < len(h); i++ {
		growth = cfg.EWMAAlpha*max(float64(h[i]-h[i-1]), 0) + (1-cfg.EWMAAlpha)*growth
	}
	ahead := float64(h[len(h)-1]) + growth*float64(cfg.LookaheadTurns)
	return ahead >= cfg.ProactiveThreshold*float64(budget)
}
