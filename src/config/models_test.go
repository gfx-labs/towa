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
	} {
		got, err := ParseChainEntry(text)
		require.NoError(t, err, text)
		require.Equal(t, want, got, text)
	}
	for _, text := range []string{
		"", "claude:claude-opus-4-7", "work/claude-opus-4-7", "claude:work/", "claude:work/#high", "claude:work/claude-opus-4-7#",
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

	for name, models := range map[string][]string{
		"an entry without an account":               {"claude:claude-opus-4-7"},
		"an account that is not configured":         {"claude:personal/claude-opus-4-7"},
		"an account of another provider":            {"claude:main/claude-opus-4-7"},
		"a provider that is not built in or set up": {"zai:work/glm-5"},
		"a second entry that is wrong":              {"claude:work/x", "claude:work/"},
		"an entry listed twice":                     {"claude:work/x#low", "claude:work/y", "claude:work/x#low"},
	} {
		require.Error(t, validateModels(models, accounts, providers), name)
	}
}
