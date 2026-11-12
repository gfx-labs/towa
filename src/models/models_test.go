package models_test

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/models"
	"github.com/gfx-labs/acpharness/src/provider"
)

// client stands for a provider's client. Chain only passes it on.
type client struct{ id string }

func (client) Stream(context.Context, provider.Request) iter.Seq2[provider.Event, error] {
	return func(func(provider.Event, error) bool) {}
}

func TestResolveAModelFromItsChain(t *testing.T) {
	// The chain of the design's sketch: two accounts on the provider's own
	// endpoint, then the same model behind a gateway under another name.
	chain := []string{"claude:work/claude-opus-4-7", "claude:personal/claude-opus-4-7", "openrouter:main/anthropic/claude-opus-4.7"}

	// The gateway's name is not in the table, so the model cannot be
	// selected until the configuration says what it is.
	_, err := models.Resolve("opus", config.Model{Chain: chain})
	var unknown *models.MetadataError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, "openrouter:main/anthropic/claude-opus-4.7", unknown.Entry)
	require.Equal(t, []string{"context_window", "max_output_tokens"}, unknown.Missing)

	// The first two entries alone resolve from the table.
	direct, err := models.Resolve("opus", config.Model{Chain: chain[:2]})
	require.NoError(t, err)
	require.Positive(t, direct.Entries[0].Metadata.ContextWindow)

	// A figure in the configuration applies to every entry, so the whole
	// chain resolves, and it replaces the table's figure.
	whole, err := models.Resolve("opus", config.Model{Chain: chain, ContextWindow: 150_000, MaxOutputTokens: 30_000})
	require.NoError(t, err)
	require.Len(t, whole.Entries, 3)
	for _, e := range whole.Entries {
		require.Equal(t, models.Metadata{ContextWindow: 150_000, MaxOutputTokens: 30_000}, e.Metadata)
	}

	clients := map[string]provider.Provider{"claude": client{"claude"}, "openrouter": client{"openrouter"}}
	lookup := func(id string) (provider.Provider, bool) { c, ok := clients[id]; return c, ok }
	entries, err := whole.Chain(lookup)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for i, e := range entries {
		// The router names an entry as the configuration wrote it.
		require.Equal(t, chain[i], e.String())
	}
	require.Equal(t, client{"openrouter"}, entries[2].Provider)

	delete(clients, "openrouter")
	_, err = whole.Chain(lookup)
	var noClient *models.ProviderError
	require.ErrorAs(t, err, &noClient)
	require.Equal(t, "openrouter", noClient.Provider)
}

// A chain may mix providers: every model gets the same tools, so a fallback
// inside a turn changes nothing the model was told.
func TestAChainMayMixProviders(t *testing.T) {
	mixed, err := models.Resolve("m", config.Model{Chain: []string{"claude:work/claude-opus-4-7", "codex:work/gpt-6.1-sol"}})
	require.NoError(t, err)
	require.Len(t, mixed.Entries, 2)
}

// Select from a real config file, as the wiring will call it.
func TestSelectFromAConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
default_model: opus
models:
  opus:
    chain: ["claude:work/claude-opus-4-7"]
    effort: medium
  glm:
    chain: ["zai:main/glm-5"]
accounts:
  - id: work
    provider: claude
    kind: oauth
  - id: main
    provider: zai
    kind: api_key
    api_key_env: ZAI_API_KEY
providers:
  - id: zai
    kind: chat
    base_url: https://zai.example/v4
`), 0o600))
	loaded, err := config.Load(config.Sources{Files: []string{path}})
	require.NoError(t, err)
	cfg := loaded.Config

	byDefault, err := models.Select(cfg.DefaultModel, cfg.Models, "")
	require.NoError(t, err)
	require.Equal(t, "opus", byDefault.Name)
	require.Equal(t, "medium", byDefault.Effort)

	// Nothing is known about this model: it fails when it is selected.
	_, err = models.Select(cfg.DefaultModel, cfg.Models, "glm")
	var unknown *models.MetadataError
	require.ErrorAs(t, err, &unknown)

	_, err = models.Select(cfg.DefaultModel, cfg.Models, "sonnet")
	var noSuch *models.UnknownModelError
	require.ErrorAs(t, err, &noSuch)

	_, err = models.Select("", nil, "")
	var none *models.NoModelError
	require.ErrorAs(t, err, &none)
}
