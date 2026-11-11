package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateModelSet(t *testing.T) {
	accounts := []Account{
		{ID: "work", Provider: ProviderClaude, Kind: AccountOAuth},
		{ID: "main", Provider: "openrouter", Kind: AccountAPIKey, APIKeyEnv: "KEY"},
	}
	providers := []Provider{{ID: "openrouter", Kind: ProviderChat, BaseURL: "https://openrouter.example/v1"}}
	chain := func(entries ...string) map[string]Model { return map[string]Model{"opus": {Chain: entries}} }

	// The design's sketch: a built-in provider, then a configured one whose
	// model name holds a slash and a colon.
	require.NoError(t, validateModelSet("opus",
		chain("claude:work/claude-opus-4-7", "openrouter:main/anthropic/claude-opus-4.7:beta"), accounts, providers))
	require.NoError(t, validateModelSet("", nil, accounts, providers))

	refused := map[string]struct {
		defaultModel string
		models       map[string]Model
	}{
		"an empty chain":                             {"opus", chain()},
		"an entry without an account":                {"opus", chain("claude:claude-opus-4-7")},
		"an entry without a provider":                {"opus", chain("work/claude-opus-4-7")},
		"an entry without a model":                   {"opus", chain("claude:work/")},
		"an account that is not configured":          {"opus", chain("claude:personal/claude-opus-4-7")},
		"an account of another provider":             {"opus", chain("claude:main/claude-opus-4-7")},
		"a provider that is not built in or set up":  {"opus", chain("zai:work/glm-5")},
		"a default that names no model":              {"gpt", chain("claude:work/claude-opus-4-7")},
		"no default beside configured models":        {"", chain("claude:work/claude-opus-4-7")},
		"a default without any model":                {"opus", nil},
		"a dialect that does not exist":              {"opus", map[string]Model{"opus": {Chain: []string{"claude:work/x"}, Dialect: "gemini"}}},
		"a negative context window":                  {"opus", map[string]Model{"opus": {Chain: []string{"claude:work/x"}, ContextWindow: -1}}},
		"a negative output limit":                    {"opus", map[string]Model{"opus": {Chain: []string{"claude:work/x"}, MaxOutputTokens: -1}}},
		"a second model that is wrong, default fine": {"opus", map[string]Model{"opus": {Chain: []string{"claude:work/x"}}, "other": {}}},
	}
	for name, tt := range refused {
		require.Error(t, validateModelSet(tt.defaultModel, tt.models, accounts, providers), name)
	}
}
