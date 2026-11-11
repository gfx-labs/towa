package config

import (
	"fmt"
	"math"
	"time"
)

// Compaction configures when a session's context is compacted and how the
// summary is asked for.
type Compaction struct {
	// Mode is when to compact: CompactionReactive, CompactionProactive,
	// CompactionSemantic or CompactionOff. Off stops the turn with a
	// context-full reason when the next request would not fit.
	Mode string `koanf:"mode" validate:"oneof=reactive proactive semantic off"`
	// SummaryPrompt selects the prompt of the summary request:
	// SummaryCheckpoint or SummaryShort.
	SummaryPrompt string `koanf:"summary_prompt" validate:"oneof=checkpoint short"`
	// MaxContextTokens caps the budget the thresholds are measured against
	// below the model's context window. Zero means the window.
	MaxContextTokens int64 `koanf:"max_context_tokens" validate:"gte=0"`
	// KeepRecentMessages is how many of the latest messages a compaction
	// leaves as they are. A message is a run of items of one role.
	KeepRecentMessages int `koanf:"keep_recent_messages" validate:"gte=0"`

	// EmergencyThreshold is the fraction of the budget at which the next
	// request is taken not to fit. The context is then cut without a model
	// summary, keeping EmergencyKeepRecentMessages.
	EmergencyThreshold          float64 `koanf:"emergency_threshold" validate:"gt=0,lte=1,gtefield=ReactiveThreshold"`
	EmergencyKeepRecentMessages int     `koanf:"emergency_keep_recent_messages" validate:"gte=0,ltefield=KeepRecentMessages"`
	// PendingWaitMillis bounds the emergency wait for an existing summary.
	// The caller's context may end it sooner.
	PendingWaitMillis int64 `koanf:"pending_wait_ms" validate:"gte=0"`

	// ReactiveThreshold is the fraction of the budget that starts a
	// compaction in reactive mode. ReactiveThresholdTokens is a token count
	// that starts one too, whichever is reached first. Zero turns the count
	// off.
	ReactiveThreshold       float64 `koanf:"reactive_threshold" validate:"gt=0,lte=1"`
	ReactiveThresholdTokens int64   `koanf:"reactive_threshold_tokens" validate:"gte=0"`

	// The proactive settings also gate semantic mode.
	//
	// ProactiveThreshold is the fraction of the budget the projected
	// context has to reach. The projection is the current context plus
	// LookaheadTurns times the growth per turn, smoothed with EWMAAlpha.
	ProactiveThreshold float64 `koanf:"proactive_threshold" validate:"gt=0,lte=1"`
	LookaheadTurns     int     `koanf:"lookahead_turns" validate:"gt=0"`
	EWMAAlpha          float64 `koanf:"ewma_alpha" validate:"gt=0,lte=1"`
	// ProactiveFloor is the fraction of the budget below which nothing is
	// projected, unless ProactiveFloorTokens is reached. Zero turns the
	// count off.
	ProactiveFloor       float64 `koanf:"proactive_floor" validate:"gte=0,lte=1"`
	ProactiveFloorTokens int64   `koanf:"proactive_floor_tokens" validate:"gte=0"`
	// MinSamples is how many per-turn context sizes are needed to project.
	MinSamples int `koanf:"min_samples" validate:"gte=2"`
	// StallWindow is how many of the latest turns must show growth. No
	// growth across them means there is nothing to get ahead of.
	StallWindow int `koanf:"stall_window" validate:"gt=0"`
	// MinTurnsBetween is how many turns pass after a compaction before
	// another is started ahead of need.
	MinTurnsBetween int `koanf:"min_turns_between" validate:"gte=0"`
	// HistoryWindow is how many per-turn context sizes are kept.
	HistoryWindow int `koanf:"history_window" validate:"gtefield=MinSamples,gtefield=StallWindow"`

	// TopicShiftThreshold is the cosine similarity between the older and
	// the newer half of the turn embeddings below which the topic is taken
	// to have changed.
	TopicShiftThreshold float64 `koanf:"topic_shift_threshold" validate:"gte=0,lte=1"`
	// RelevanceKeepThreshold is the cosine similarity to the current goal
	// at or above which an older message is kept and not summarised.
	RelevanceKeepThreshold float64 `koanf:"relevance_keep_threshold" validate:"gte=0,lte=1"`
	// GoalWindowMessages is how many of the latest messages make up the
	// current goal.
	GoalWindowMessages int `koanf:"goal_window_messages" validate:"gt=0"`
	// EmbedMaxChars is how much of a message is embedded, from its start.
	EmbedMaxChars int `koanf:"embed_max_chars" validate:"gt=0"`
	// EmbeddingHistoryWindow is how many turn embeddings are kept for
	// topic-shift detection.
	EmbeddingHistoryWindow int `koanf:"embedding_history_window" validate:"gte=4"`

	// OpenAINative is how a conversation with an OpenAI Responses endpoint
	// is compacted: NativeAuto lets the server compact at
	// OpenAINativeThresholdTokens, NativeExplicit calls the compact
	// endpoint at that count, NativeOff uses Mode as for any provider.
	OpenAINative                string `koanf:"openai_native" validate:"oneof=auto explicit off"`
	OpenAINativeThresholdTokens int64  `koanf:"openai_native_threshold_tokens"`
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

// Every figure is jcode's, read from origin/master 8e11eb23e with
// `git show`. "config-types" is crates/jcode-config-types/src/lib.rs and
// "core" is crates/jcode-compaction-core/src/lib.rs. jcode counts the keep
// and goal windows in messages and calls them turns. The key names here say
// messages.
var compactionDefaults = map[string]any{
	"compaction.mode": CompactionReactive, // config-types:350
	// docs/design.md makes checkpoint the default. jcode has only the
	// short prompt.
	"compaction.summary_prompt":                 SummaryCheckpoint,
	"compaction.max_context_tokens":             0,      // config-types:363
	"compaction.keep_recent_messages":           10,     // core:19 RECENT_TURNS_TO_KEEP
	"compaction.emergency_threshold":            0.95,   // core:13 CRITICAL_THRESHOLD
	"compaction.emergency_keep_recent_messages": 2,      // core:22 MIN_TURNS_TO_KEEP
	"compaction.pending_wait_ms":                15000,  // crates/jcode-base/src/compaction.rs:44 HARD_THRESHOLD_PENDING_WAIT_MS
	"compaction.reactive_threshold":             0.80,   // config-types:351
	"compaction.reactive_threshold_tokens":      400000, // config-types:352
	// jcode projects against the constant COMPACTION_THRESHOLD and has no
	// setting for it.
	"compaction.proactive_threshold":            0.80,       // core:9
	"compaction.lookahead_turns":                15,         // config-types:353
	"compaction.ewma_alpha":                     0.3,        // config-types:354
	"compaction.proactive_floor":                0.40,       // config-types:355
	"compaction.proactive_floor_tokens":         150000,     // config-types:356
	"compaction.min_samples":                    3,          // config-types:357
	"compaction.stall_window":                   5,          // config-types:358
	"compaction.min_turns_between":              10,         // config-types:359
	"compaction.history_window":                 20,         // core:74 TOKEN_HISTORY_WINDOW
	"compaction.topic_shift_threshold":          0.45,       // config-types:360
	"compaction.relevance_keep_threshold":       0.65,       // config-types:361
	"compaction.goal_window_messages":           5,          // config-types:362
	"compaction.embed_max_chars":                512,        // core:77 EMBED_MAX_CHARS_PER_MSG
	"compaction.embedding_history_window":       10,         // core:80 EMBEDDING_HISTORY_WINDOW
	"compaction.openai_native":                  NativeAuto, // config-types:1320
	"compaction.openai_native_threshold_tokens": 200000,     // config-types:1321
}

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
