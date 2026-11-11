package config

import (
	"fmt"
	"math"
	"time"
)

// Compaction configures when a session's context is compacted and how the
// summary is asked for.
//
// Every default is jcode's, read from origin/master 8e11eb23e with
// `git show`, and each field names its line. "config-types" is
// crates/jcode-config-types/src/lib.rs and "core" is
// crates/jcode-compaction-core/src/lib.rs. jcode counts the keep and goal
// windows in messages and calls them turns. The key names here say
// messages.
type Compaction struct {
	// Mode is when to compact: CompactionReactive, CompactionProactive,
	// CompactionSemantic or CompactionOff. Off stops the turn with a
	// context-full reason when the next request would not fit.
	// config-types:350.
	Mode string `name:"mode" default:"reactive" validate:"oneof=reactive proactive semantic off" help:"When to compact: reactive, proactive, semantic or off. Off stops the turn when the next request would not fit."`
	// SummaryPrompt selects the prompt of the summary request:
	// SummaryCheckpoint or SummaryShort. docs/design.md makes checkpoint
	// the default. jcode has only the short prompt.
	SummaryPrompt string `name:"summary_prompt" default:"checkpoint" validate:"oneof=checkpoint short" help:"Prompt of the summary request: checkpoint or short."`
	// MaxContextTokens caps the budget the thresholds are measured against
	// below the model's context window. Zero means the window.
	// config-types:363.
	MaxContextTokens int64 `name:"max_context_tokens" default:"0" validate:"gte=0" help:"Budget the thresholds are measured against, when lower than the model's context window. 0 is the window."`
	// KeepRecentMessages is how many of the latest messages a compaction
	// leaves as they are. A message is a run of items of one role.
	// core:19 RECENT_TURNS_TO_KEEP.
	KeepRecentMessages int `name:"keep_recent_messages" default:"10" validate:"gte=0" help:"Latest messages a compaction leaves as they are."`

	// EmergencyThreshold is the fraction of the budget at which the next
	// request is taken not to fit. The context is then cut without a model
	// summary, keeping EmergencyKeepRecentMessages. core:13
	// CRITICAL_THRESHOLD and core:22 MIN_TURNS_TO_KEEP.
	EmergencyThreshold          float64 `name:"emergency_threshold" default:"0.95" validate:"gt=0,lte=1,gtefield=ReactiveThreshold" help:"Fraction of the budget at which the context is cut without a model summary."`
	EmergencyKeepRecentMessages int     `name:"emergency_keep_recent_messages" default:"2" validate:"gte=0,ltefield=KeepRecentMessages" help:"Latest messages an emergency cut keeps."`
	// PendingWaitMillis bounds the emergency wait for an existing summary.
	// The caller's context may end it sooner.
	// crates/jcode-base/src/compaction.rs:44 HARD_THRESHOLD_PENDING_WAIT_MS.
	PendingWaitMillis int64 `name:"pending_wait_ms" default:"15000" validate:"gte=0" help:"Milliseconds an emergency cut waits for a summary that is already being made."`

	// ReactiveThreshold is the fraction of the budget that starts a
	// compaction in reactive mode. ReactiveThresholdTokens is a token count
	// that starts one too, whichever is reached first. Zero turns the count
	// off. config-types:351 and 352.
	ReactiveThreshold       float64 `name:"reactive_threshold" default:"0.8" validate:"gt=0,lte=1" help:"Fraction of the budget that starts a compaction in reactive mode."`
	ReactiveThresholdTokens int64   `name:"reactive_threshold_tokens" default:"400000" validate:"gte=0" help:"Token count that starts a compaction in reactive mode. 0 turns the count off."`

	// The proactive settings also gate semantic mode.
	//
	// ProactiveThreshold is the fraction of the budget the projected
	// context has to reach. The projection is the current context plus
	// LookaheadTurns times the growth per turn, smoothed with EWMAAlpha.
	// jcode projects against the constant COMPACTION_THRESHOLD (core:9) and
	// has no setting for it. config-types:353 and 354.
	ProactiveThreshold float64 `name:"proactive_threshold" default:"0.8" validate:"gt=0,lte=1" help:"Fraction of the budget the projected context has to reach to compact ahead of need."`
	LookaheadTurns     int     `name:"lookahead_turns" default:"15" validate:"gt=0" help:"Turns the projection looks ahead."`
	EWMAAlpha          float64 `name:"ewma_alpha" default:"0.3" validate:"gt=0,lte=1" help:"Weight of the latest turn in the smoothed growth per turn."`
	// ProactiveFloor is the fraction of the budget below which nothing is
	// projected, unless ProactiveFloorTokens is reached. Zero turns the
	// count off. config-types:355 and 356.
	ProactiveFloor       float64 `name:"proactive_floor" default:"0.4" validate:"gte=0,lte=1" help:"Fraction of the budget below which nothing is projected."`
	ProactiveFloorTokens int64   `name:"proactive_floor_tokens" default:"150000" validate:"gte=0" help:"Token count at which projection starts below the floor. 0 turns the count off."`
	// MinSamples is how many per-turn context sizes are needed to project.
	// config-types:357.
	MinSamples int `name:"min_samples" default:"3" validate:"gte=2" help:"Per-turn context sizes needed to project."`
	// StallWindow is how many of the latest turns must show growth. No
	// growth across them means there is nothing to get ahead of.
	// config-types:358.
	StallWindow int `name:"stall_window" default:"5" validate:"gt=0" help:"Latest turns that must show growth for a projection."`
	// MinTurnsBetween is how many turns pass after a compaction before
	// another is started ahead of need. config-types:359.
	MinTurnsBetween int `name:"min_turns_between" default:"10" validate:"gte=0" help:"Turns that pass after a compaction before another is started ahead of need."`
	// HistoryWindow is how many per-turn context sizes are kept. core:74
	// TOKEN_HISTORY_WINDOW.
	HistoryWindow int `name:"history_window" default:"20" validate:"gtefield=MinSamples,gtefield=StallWindow" help:"Per-turn context sizes kept."`

	// TopicShiftThreshold is the cosine similarity between the older and
	// the newer half of the turn embeddings below which the topic is taken
	// to have changed. config-types:360.
	TopicShiftThreshold float64 `name:"topic_shift_threshold" default:"0.45" validate:"gte=0,lte=1" help:"Cosine similarity between older and newer turns below which the topic is taken to have changed."`
	// RelevanceKeepThreshold is the cosine similarity to the current goal
	// at or above which an older message is kept and not summarised.
	// config-types:361.
	RelevanceKeepThreshold float64 `name:"relevance_keep_threshold" default:"0.65" validate:"gte=0,lte=1" help:"Cosine similarity to the current goal at or above which an older message is kept."`
	// GoalWindowMessages is how many of the latest messages make up the
	// current goal. config-types:362.
	GoalWindowMessages int `name:"goal_window_messages" default:"5" validate:"gt=0" help:"Latest messages that make up the current goal."`
	// EmbedMaxChars is how much of a message is embedded, from its start.
	// core:77 EMBED_MAX_CHARS_PER_MSG.
	EmbedMaxChars int `name:"embed_max_chars" default:"512" validate:"gt=0" help:"Characters of a message that are embedded, from its start."`
	// EmbeddingHistoryWindow is how many turn embeddings are kept for
	// topic-shift detection. core:80 EMBEDDING_HISTORY_WINDOW.
	EmbeddingHistoryWindow int `name:"embedding_history_window" default:"10" validate:"gte=4" help:"Turn embeddings kept for topic-shift detection."`

	// OpenAINative is how a conversation with an OpenAI Responses endpoint
	// is compacted: NativeAuto lets the server compact at
	// OpenAINativeThresholdTokens, NativeExplicit calls the compact
	// endpoint at that count, NativeOff uses Mode as for any provider.
	// config-types:1320 and 1321.
	OpenAINative                string `name:"openai_native" default:"auto" validate:"oneof=auto explicit off" help:"How a conversation with an OpenAI Responses endpoint is compacted: auto, explicit or off."`
	OpenAINativeThresholdTokens int64  `name:"openai_native_threshold_tokens" default:"200000" help:"Token count at which an OpenAI Responses conversation is compacted natively."`
}

// Compaction modes.
const (
	CompactionReactive  = "reactive"
	CompactionProactive = "proactive"
	CompactionSemantic  = "semantic"
	CompactionOff       = "off"
)

// Summary prompts.
const (
	SummaryCheckpoint = "checkpoint"
	SummaryShort      = "short"
)

// OpenAI native compaction modes.
const (
	NativeAuto     = "auto"
	NativeExplicit = "explicit"
	NativeOff      = "off"
)

func (c Compaction) validate() error {
	if err := validateFields("compaction", c); err != nil {
		return err
	}
	switch c.OpenAINative {
	case NativeAuto, NativeExplicit:
		if c.OpenAINativeThresholdTokens <= 0 {
			return fmt.Errorf("compaction.openai_native_threshold_tokens: %d is not positive, and compaction.openai_native %q needs it",
				c.OpenAINativeThresholdTokens, c.OpenAINative)
		}
	}
	if c.PendingWaitMillis > math.MaxInt64/int64(time.Millisecond) {
		return fmt.Errorf("compaction.pending_wait_ms: %d overflows a duration", c.PendingWaitMillis)
	}
	return nil
}

// Validate checks a compaction section passed directly to a controller,
// outside the configuration loader.
func (c Compaction) Validate() error { return c.validate() }
