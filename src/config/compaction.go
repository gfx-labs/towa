package config

import "fmt"

// Compaction configures when a session's context is compacted and how the
// summary is asked for.
type Compaction struct {
	// Mode is when to compact: CompactionReactive, CompactionProactive,
	// CompactionSemantic or CompactionOff. Off stops the turn with a
	// context-full reason when the next request would not fit.
	Mode string `koanf:"mode"`
	// SummaryPrompt selects the prompt of the summary request:
	// SummaryCheckpoint or SummaryShort.
	SummaryPrompt string `koanf:"summary_prompt"`
	// MaxContextTokens caps the budget the thresholds are measured against
	// below the model's context window. Zero means the window.
	MaxContextTokens int64 `koanf:"max_context_tokens"`
	// KeepRecentMessages is how many of the latest messages a compaction
	// leaves as they are. A message is a run of items of one role.
	KeepRecentMessages int `koanf:"keep_recent_messages"`

	// EmergencyThreshold is the fraction of the budget at which the next
	// request is taken not to fit. The context is then cut without a model
	// summary, keeping EmergencyKeepRecentMessages.
	EmergencyThreshold          float64 `koanf:"emergency_threshold"`
	EmergencyKeepRecentMessages int     `koanf:"emergency_keep_recent_messages"`
	// PendingWaitMillis bounds the emergency wait for an existing summary.
	// The caller's context may end it sooner.
	PendingWaitMillis int64 `koanf:"pending_wait_ms"`

	// ReactiveThreshold is the fraction of the budget that starts a
	// compaction in reactive mode. ReactiveThresholdTokens is a token count
	// that starts one too, whichever is reached first. Zero turns the count
	// off.
	ReactiveThreshold       float64 `koanf:"reactive_threshold"`
	ReactiveThresholdTokens int64   `koanf:"reactive_threshold_tokens"`

	// The proactive settings also gate semantic mode.
	//
	// ProactiveThreshold is the fraction of the budget the projected
	// context has to reach. The projection is the current context plus
	// LookaheadTurns times the growth per turn, smoothed with EWMAAlpha.
	ProactiveThreshold float64 `koanf:"proactive_threshold"`
	LookaheadTurns     int     `koanf:"lookahead_turns"`
	EWMAAlpha          float64 `koanf:"ewma_alpha"`
	// ProactiveFloor is the fraction of the budget below which nothing is
	// projected, unless ProactiveFloorTokens is reached. Zero turns the
	// count off.
	ProactiveFloor       float64 `koanf:"proactive_floor"`
	ProactiveFloorTokens int64   `koanf:"proactive_floor_tokens"`
	// MinSamples is how many per-turn context sizes are needed to project.
	MinSamples int `koanf:"min_samples"`
	// StallWindow is how many of the latest turns must show growth. No
	// growth across them means there is nothing to get ahead of.
	StallWindow int `koanf:"stall_window"`
	// MinTurnsBetween is how many turns pass after a compaction before
	// another is started ahead of need.
	MinTurnsBetween int `koanf:"min_turns_between"`
	// HistoryWindow is how many per-turn context sizes are kept.
	HistoryWindow int `koanf:"history_window"`

	// TopicShiftThreshold is the cosine similarity between the older and
	// the newer half of the turn embeddings below which the topic is taken
	// to have changed.
	TopicShiftThreshold float64 `koanf:"topic_shift_threshold"`
	// RelevanceKeepThreshold is the cosine similarity to the current goal
	// at or above which an older message is kept and not summarised.
	RelevanceKeepThreshold float64 `koanf:"relevance_keep_threshold"`
	// GoalWindowMessages is how many of the latest messages make up the
	// current goal.
	GoalWindowMessages int `koanf:"goal_window_messages"`
	// EmbedMaxChars is how much of a message is embedded, from its start.
	EmbedMaxChars int `koanf:"embed_max_chars"`
	// EmbeddingHistoryWindow is how many turn embeddings are kept for
	// topic-shift detection.
	EmbeddingHistoryWindow int `koanf:"embedding_history_window"`

	// OpenAINative is how a conversation with an OpenAI Responses endpoint
	// is compacted: NativeAuto lets the server compact at
	// OpenAINativeThresholdTokens, NativeExplicit calls the compact
	// endpoint at that count, NativeOff uses Mode as for any provider.
	OpenAINative                string `koanf:"openai_native"`
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
	switch c.Mode {
	case CompactionReactive, CompactionProactive, CompactionSemantic, CompactionOff:
	default:
		return fmt.Errorf("compaction.mode: %q is not %q, %q, %q or %q",
			c.Mode, CompactionReactive, CompactionProactive, CompactionSemantic, CompactionOff)
	}
	switch c.SummaryPrompt {
	case SummaryCheckpoint, SummaryShort:
	default:
		return fmt.Errorf("compaction.summary_prompt: %q is not %q or %q", c.SummaryPrompt, SummaryCheckpoint, SummaryShort)
	}
	switch c.OpenAINative {
	case NativeAuto, NativeExplicit:
		if c.OpenAINativeThresholdTokens <= 0 {
			return fmt.Errorf("compaction.openai_native_threshold_tokens: %d is not positive, and compaction.openai_native %q needs it",
				c.OpenAINativeThresholdTokens, c.OpenAINative)
		}
	case NativeOff:
	default:
		return fmt.Errorf("compaction.openai_native: %q is not %q, %q or %q", c.OpenAINative, NativeAuto, NativeExplicit, NativeOff)
	}
	// A fraction of the budget. Above 1 it could never be reached.
	for key, value := range map[string]float64{
		"compaction.emergency_threshold": c.EmergencyThreshold,
		"compaction.reactive_threshold":  c.ReactiveThreshold,
		"compaction.proactive_threshold": c.ProactiveThreshold,
		"compaction.ewma_alpha":          c.EWMAAlpha,
	} {
		if value <= 0 || value > 1 {
			return fmt.Errorf("%s: %v is not above 0 and at most 1", key, value)
		}
	}
	// The floor may be 0, which projects from the first turn. A similarity
	// is a cosine.
	for key, value := range map[string]float64{
		"compaction.proactive_floor":          c.ProactiveFloor,
		"compaction.topic_shift_threshold":    c.TopicShiftThreshold,
		"compaction.relevance_keep_threshold": c.RelevanceKeepThreshold,
	} {
		if value < 0 || value > 1 {
			return fmt.Errorf("%s: %v is not between 0 and 1", key, value)
		}
	}
	for key, value := range map[string]int64{
		"compaction.pending_wait_ms":                c.PendingWaitMillis,
		"compaction.max_context_tokens":             c.MaxContextTokens,
		"compaction.reactive_threshold_tokens":      c.ReactiveThresholdTokens,
		"compaction.proactive_floor_tokens":         c.ProactiveFloorTokens,
		"compaction.keep_recent_messages":           int64(c.KeepRecentMessages),
		"compaction.emergency_keep_recent_messages": int64(c.EmergencyKeepRecentMessages),
		"compaction.min_turns_between":              int64(c.MinTurnsBetween),
	} {
		if value < 0 {
			return fmt.Errorf("%s: %d is negative", key, value)
		}
	}
	for key, value := range map[string]int{
		"compaction.lookahead_turns":      c.LookaheadTurns,
		"compaction.stall_window":         c.StallWindow,
		"compaction.goal_window_messages": c.GoalWindowMessages,
		"compaction.embed_max_chars":      c.EmbedMaxChars,
	} {
		if value <= 0 {
			return fmt.Errorf("%s: %d is not positive", key, value)
		}
	}
	// A growth needs two sizes, and the two halves compared for a topic
	// shift need two embeddings each.
	if c.MinSamples < 2 {
		return fmt.Errorf("compaction.min_samples: %d is below 2", c.MinSamples)
	}
	if c.HistoryWindow < c.MinSamples || c.HistoryWindow < c.StallWindow {
		return fmt.Errorf("compaction.history_window: %d is below compaction.min_samples or compaction.stall_window", c.HistoryWindow)
	}
	if c.EmbeddingHistoryWindow < 4 {
		return fmt.Errorf("compaction.embedding_history_window: %d is below 4", c.EmbeddingHistoryWindow)
	}
	if c.EmergencyThreshold < c.ReactiveThreshold {
		return fmt.Errorf("compaction.emergency_threshold: %v is below compaction.reactive_threshold %v",
			c.EmergencyThreshold, c.ReactiveThreshold)
	}
	if c.EmergencyKeepRecentMessages > c.KeepRecentMessages {
		return fmt.Errorf("compaction.emergency_keep_recent_messages: %d is above compaction.keep_recent_messages %d",
			c.EmergencyKeepRecentMessages, c.KeepRecentMessages)
	}
	return nil
}
