package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseChainEntry(t *testing.T) {
	for text, want := range map[string]ChainEntry{
		"claude:work/claude-opus-4-7":        {Provider: "claude", Account: "work", Model: "claude-opus-4-7"},
		"claude:work/claude-opus-4-7#medium": {Provider: "claude", Account: "work", Model: "claude-opus-4-7", Effort: "medium"},
		// A gateway's model name holds a slash and a colon. The effort is
		// not checked: it is the provider's word.
		"openrouter:main/anthropic/claude-opus-4.7:beta#x-high": {Provider: "openrouter", Account: "main", Model: "anthropic/claude-opus-4.7:beta", Effort: "x-high"},
		// Without an account, every account of the provider.
		"claude:claude-opus-4-7#high": {Provider: "claude", Account: AnyAccount, Model: "claude-opus-4-7", Effort: "high"},
		"claude:*/claude-opus-4-7":    {Provider: "claude", Account: AnyAccount, Model: "claude-opus-4-7"},
		"lmstudio:openai/gpt-oss-20b": {Provider: "lmstudio", Account: "openai", Model: "gpt-oss-20b"},
	} {
		got, err := ParseChainEntry(text)
		require.NoError(t, err, text)
		require.Equal(t, want, got, text)
	}
	for _, text := range []string{
		"", "claude-opus-4-7", "work/claude-opus-4-7", ":claude-opus-4-7", "claude:work/", "claude:work/#high", "claude:work/claude-opus-4-7#",
	} {
		_, err := ParseChainEntry(text)
		require.Error(t, err, text)
	}
}

func TestValidateModels(t *testing.T) {
	accounts := []Account{
		{ID: "work", Provider: ProviderClaude, Kind: AccountOAuth},
		{ID: "main", Provider: "openrouter", Kind: AccountAPIKey, APIKeyEnv: "KEY"},
	}
	providers := []Provider{{ID: "openrouter", Kind: ProviderChat, BaseURL: "https://openrouter.example/v1"}}

	require.NoError(t, validateModels(
		[]string{"claude:work/claude-opus-4-7#high", "claude:work/claude-opus-4-7", "openrouter:main/anthropic/claude-opus-4.7:beta"},
		accounts, providers))
	require.NoError(t, validateModels(nil, accounts, providers))
	require.NoError(t, validateModels([]string{"claude:claude-opus-4-7", "openrouter:*/glm-5"}, accounts, providers))

	for name, models := range map[string][]string{
		"an account that is not configured":         {"claude:personal/claude-opus-4-7"},
		"an account of another provider":            {"claude:main/claude-opus-4-7"},
		"a provider that is not built in or set up": {"zai:work/glm-5"},
		"a second entry that is wrong":              {"claude:work/x", "claude:work/"},
		"an entry listed twice":                     {"claude:work/x#low", "claude:work/y", "claude:work/x#low"},
	} {
		require.Error(t, validateModels(models, accounts, providers), name)
	}
}

func TestResolveModel(t *testing.T) {
	configured := []string{
		"claude:work/claude-opus-4-7#high",
		"claude:personal/claude-opus-4-7#high",
		"claude:work/claude-sonnet-4-6",
		"codex:chatgpt/claude-sonnet-4-6",
		"codex:*/gpt-6.1-sol#high",
		"codex:*/gpt-6.1-sol#low",
		"lmstudio:none/llama3.2:3b",
	}
	for name, want := range map[string]string{
		// An entry as written selects itself.
		"claude:personal/claude-opus-4-7#high": "claude:personal/claude-opus-4-7#high",
		// Matches that differ only in account take the first: the rest are
		// its fallbacks.
		"claude-opus-4-7":          "claude:work/claude-opus-4-7#high",
		"claude:claude-opus-4-7":   "claude:work/claude-opus-4-7#high",
		"claude:*/claude-opus-4-7": "claude:work/claude-opus-4-7#high",
		"claude-sonnet-4-6":        "",
		"claude:claude-sonnet-4-6": "claude:work/claude-sonnet-4-6",
		"gpt-6.1-sol#low":          "codex:*/gpt-6.1-sol#low",
		"codex:gpt-6.1-sol#high":   "codex:*/gpt-6.1-sol#high",
		// The colon is the model's own: no provider is named lmstudio:llama3.2.
		"llama3.2:3b": "lmstudio:none/llama3.2:3b",
	} {
		got, err := ResolveModel(configured, name)
		if want == "" {
			var ambiguous *AmbiguousModelError
			require.ErrorAs(t, err, &ambiguous, name)
			require.Equal(t, []string{"claude:work/claude-sonnet-4-6", "codex:chatgpt/claude-sonnet-4-6"}, ambiguous.Matches)
			continue
		}
		require.NoError(t, err, name)
		require.Equal(t, want, got, name)
	}
	// Two efforts are two choices.
	var ambiguous *AmbiguousModelError
	_, err := ResolveModel(configured, "gpt-6.1-sol")
	require.ErrorAs(t, err, &ambiguous)
	for _, name := range []string{"glm-5", "claude-opus-4-7#low", "codex:claude-opus-4-7", "claude:other/claude-opus-4-7"} {
		_, err := ResolveModel(configured, name)
		var unknown *UnknownModelError
		require.ErrorAs(t, err, &unknown, name)
	}
}
