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

func TestSelectResolvesAModelAndTheOnesAfterIt(t *testing.T) {
	// Two accounts on the provider's own endpoint, then the same model
	// behind a gateway under another name.
	list := []string{"claude:work/claude-opus-4-7#high", "claude:personal/claude-opus-4-7", "openrouter:main/anthropic/claude-opus-4.7#low"}

	// The gateway's name is not in the table, so no model that can fall
	// back to it is usable until the configuration says what it is.
	_, err := models.Select(list, models.Metadata{}, "")
	var unknown *models.MetadataError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, list[2], unknown.Entry)
	require.Equal(t, []string{config.KeyModelContextWindow, config.KeyModelMaxOutputTokens}, unknown.Missing)

	// The first two entries alone resolve from the table.
	direct, err := models.Select(list[:2], models.Metadata{}, "")
	require.NoError(t, err)
	require.Positive(t, direct.Entries[0].Metadata.ContextWindow)

	// The configured figures are those of a model the table does not
	// have. A model the table has keeps the table's.
	unlisted := models.Metadata{ContextWindow: 150_000, MaxOutputTokens: 30_000}
	whole, err := models.Select(list, unlisted, "")
	require.NoError(t, err)
	require.Equal(t, list[0], whole.Name)
	require.Len(t, whole.Entries, 3)
	require.Equal(t, direct.Entries[0].Metadata, whole.Entries[0].Metadata)
	require.Equal(t, unlisted, whole.Entries[2].Metadata)
	// Each entry has its own effort, and one without sends none.
	require.Equal(t, []string{"high", "", "low"}, []string{whole.Entries[0].Effort, whole.Entries[1].Effort, whole.Entries[2].Effort})

	// A model chosen by its text falls back only to the entries after it.
	second, err := models.Select(list, unlisted, list[1])
	require.NoError(t, err)
	require.Equal(t, list[1], second.Name)
	require.Len(t, second.Entries, 2)
	require.Equal(t, "personal", second.Entries[0].Account)

	clients := map[string]provider.Provider{"claude": client{"claude"}, "openrouter": client{"openrouter"}}
	lookup := func(id string) (provider.Provider, bool) { c, ok := clients[id]; return c, ok }
	entries, err := whole.Chain(lookup)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, client{"openrouter"}, entries[2].Provider)
	require.Equal(t, "low", entries[2].Effort)
	require.Equal(t, "openrouter:main/anthropic/claude-opus-4.7", entries[2].String())

	delete(clients, "openrouter")
	_, err = whole.Chain(lookup)
	var noClient *models.ProviderError
	require.ErrorAs(t, err, &noClient)
	require.Equal(t, "openrouter", noClient.Provider)

	_, err = models.Select(list, unlisted, "claude:work/claude-sonnet-4-6")
	var noSuch *models.UnknownModelError
	require.ErrorAs(t, err, &noSuch)

	_, err = models.Select(nil, unlisted, "")
	var none *models.NoModelError
	require.ErrorAs(t, err, &none)
}

// Select from a real config file and from the flag, as the wiring calls it.
func TestSelectFromAConfigFileAndAFlag(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(`
models:
  - claude:work/claude-opus-4-7#medium
  - zai:main/glm-5
model_context_window: 128000
model_max_output_tokens: 8000
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
	unlisted := models.Metadata{ContextWindow: cfg.ModelContextWindow, MaxOutputTokens: cfg.ModelMaxOutputTokens}

	first, err := models.Select(cfg.Models, unlisted, "")
	require.NoError(t, err)
	require.Equal(t, "claude-opus-4-7", first.Entries[0].Model)
	require.Equal(t, "medium", first.Entries[0].Effort)
	require.Equal(t, unlisted, first.Entries[1].Metadata)

	// A flag lists the models separated by commas, and replaces the file's.
	loaded, err = config.Load(config.Sources{Files: []string{path}}, "--models", "zai:main/glm-5#high,claude:work/claude-opus-4-7")
	require.NoError(t, err)
	require.Equal(t, []string{"zai:main/glm-5#high", "claude:work/claude-opus-4-7"}, loaded.Config.Models)
}
