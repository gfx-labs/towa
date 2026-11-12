package models

// builtin holds the models whose figures were read from a source, by the
// provider's own model id. A model that is not here needs its figures in
// the configuration (model_context_window and model_max_output_tokens).
// Add a row only with the source it was read from.
//
// A name a gateway gives a model ("anthropic/claude-opus-4.7" on
// OpenRouter) is a different id and is not matched: the window that counts
// there is the gateway's, which this table does not know.
//
// Sources, all read on 2026-11-11:
//
//	[A] Anthropic, "Models overview", the "Compare models" table (context
//	    window and max output per Claude API ID):
//	    https://docs.claude.com/en/docs/about-claude/models/overview
//	[O] OpenAI, "Models", the flagship model cards (context window and max
//	    output per model ID): https://developers.openai.com/api/docs/models
//	    The page gives the context window rounded, as "1.05M".
//	[J] jcode origin/master 8e11eb23e,
//	    crates/jcode-provider-core/src/anthropic.rs: anthropic_context_mode
//	    (the default window: 1M from Opus 4.7, 200K for 4.6 without the
//	    context-1m beta and for Haiku 4.5), which
//	    anthropic_context_mode_is_verified says was probed against the live
//	    API for these generations, and anthropic_max_output_tokens (128K
//	    and, for Haiku 4.5, 64K, "per the published model comparison
//	    table").
//	[P] Probed on 2026-11-12 with a subscription login: each Claude model
//	    here was sent thinking {"type":"adaptive","display":"summarized"}.
//	    Haiku 4.5 alone answered HTTP 400 "adaptive thinking is not
//	    supported on this model"
//	    (docs/notes/2026-11-12-claude-thinking.md).
var builtin = map[string]Metadata{
	"claude-fable-5-2":  {ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, // [A]
	"claude-opus-5-5":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, // [A]
	"claude-sonnet-5-5": {ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, // [A]
	"claude-haiku-5-5":  {ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, // [A]

	"claude-opus-4-8":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},                        // [J]
	"claude-opus-4-7":   {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},                        // [J]
	"claude-opus-4-6":   {ContextWindow: 200_000, MaxOutputTokens: 128_000},                          // [J]
	"claude-sonnet-4-6": {ContextWindow: 200_000, MaxOutputTokens: 128_000},                          // [J]
	"claude-haiku-4-5":  {ContextWindow: 200_000, MaxOutputTokens: 64_000, NoAdaptiveThinking: true}, // [J] [P]

	"gpt-6-astra":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6.1-sol":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6.1-terra": {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6-luna":    {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
}
