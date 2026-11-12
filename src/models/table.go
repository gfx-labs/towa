package models

// builtin holds the models a provider's model list does not settle, by the
// provider's own model id. Every other model takes its figures from the
// configuration (model_context_window and model_max_output_tokens), then
// the provider's model list (see Lists), then the row its name points to
// (see guess). Add a row only for such an exception, with the source it was
// read from.
//
// No Claude model is here: Anthropic's list gives both figures and whether
// the model takes adaptive thinking.
//
// The OpenAI models are here because no list gives their output limit: the
// Codex backend's gives a context window alone, and the API key endpoint's
// gives neither figure.
//
// A name a gateway gives a model ("openai/gpt-6.1-sol" on OpenRouter) is a
// different id and is not matched: the window that counts there is the
// gateway's, which this table does not know.
//
// Source, read on 2026-11-11:
//
//	[O] OpenAI, "Models", the flagship model cards (context window and max
//	    output per model ID): https://developers.openai.com/api/docs/models
//	    The page gives the context window rounded, as "1.05M".
var builtin = map[string]Metadata{
	"gpt-6-astra":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6.1-sol":   {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6.1-terra": {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
	"gpt-6-luna":    {ContextWindow: 1_050_000, MaxOutputTokens: 128_000}, // [O]
}
