package models_test

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/models"
	"github.com/gfx-labs/towa/src/provider"
)

// client stands for a provider's client. Chain only passes it on.
type client struct{ id string }

func (client) Stream(context.Context, provider.Request) iter.Seq2[provider.Event, error] {
	return func(func(provider.Event, error) bool) {}
}

func TestSelectResolvesAModelAndTheOnesAfterIt(t *testing.T) {
	// Two accounts on the provider's own endpoint, then the same model
	// behind a gateway under another name.
	list := []string{"codex:work/gpt-6.1-sol#high", "codex:personal/gpt-6.1-sol", "openrouter:main/openai/gpt-6.1-sol#low"}

	// The gateway's name is not in the table, so no model that can fall
	// back to it is usable until the configuration says what it is.
	_, err := models.Select(list, models.Unlisted{}, "")
	var unknown *models.MetadataError
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, list[2], unknown.Entry)
	require.Equal(t, []string{config.KeyModelContextWindow, config.KeyModelMaxOutputTokens}, unknown.Missing)

	// The first two entries alone resolve from the table.
	direct, err := models.Select(list[:2], models.Unlisted{}, "")
	require.NoError(t, err)
	require.Positive(t, direct.Entries[0].Metadata.ContextWindow)

	// The configured figures are those of a model the table does not
	// have. A model the table has keeps the table's.
	unlisted := models.Unlisted{Configured: models.Metadata{ContextWindow: 150_000, MaxOutputTokens: 30_000}}
	whole, err := models.Select(list, unlisted, "")
	require.NoError(t, err)
	require.Equal(t, list[0], whole.Name)
	require.Len(t, whole.Entries, 3)
	require.Equal(t, direct.Entries[0].Metadata, whole.Entries[0].Metadata)
	require.Equal(t, unlisted.Configured, whole.Entries[2].Metadata)
	// Each entry has its own effort, and one without sends none.
	require.Equal(t, []string{"high", "", "low"}, []string{whole.Entries[0].Effort, whole.Entries[1].Effort, whole.Entries[2].Effort})

	// A model chosen by its text falls back only to the entries after it.
	second, err := models.Select(list, unlisted, list[1])
	require.NoError(t, err)
	require.Equal(t, list[1], second.Name)
	require.Len(t, second.Entries, 2)
	require.Equal(t, "personal", second.Entries[0].Account)

	clients := map[string]provider.Provider{"codex": client{"codex"}, "openrouter": client{"openrouter"}}
	lookup := func(id string) (provider.Provider, bool) { c, ok := clients[id]; return c, ok }
	entries, err := whole.Chain(lookup)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	require.Equal(t, client{"openrouter"}, entries[2].Provider)
	require.Equal(t, "low", entries[2].Effort)
	require.Equal(t, "openrouter:main/openai/gpt-6.1-sol", entries[2].String())

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

// A model that came out after the table was written works by its name.
func TestSelectGuessesANewModelFromItsName(t *testing.T) {
	figures := func(model string) (models.Metadata, string, error) {
		m, err := models.Select([]string{"codex:work/" + model}, models.Unlisted{}, "")
		if err != nil {
			return models.Metadata{}, "", err
		}
		return m.Entries[0].Metadata, m.Entries[0].GuessedFrom, nil
	}
	for model, row := range map[string]string{
		"gpt-7-sol":            "gpt-6.1-sol", // a newer version of a known family
		"gpt-6.1-sol-20261001": "gpt-6.1-sol", // a known model with its release date
		"gpt-6.5-luna":         "gpt-6-luna",
	} {
		want, from, err := figures(row)
		require.NoError(t, err)
		require.Empty(t, from)
		got, from, err := figures(model)
		require.NoError(t, err, model)
		require.Equal(t, want, got, model)
		require.Equal(t, row, from, model)
	}

	// A family the table does not have takes what the vendor's newest
	// models agree on.
	newest, _, err := figures("gpt-6.1-sol")
	require.NoError(t, err)
	got, from, err := figures("gpt-7-nova")
	require.NoError(t, err)
	require.Equal(t, newest, got)
	require.NotEmpty(t, from)

	// Nothing says an older model, another vendor's model or a name
	// without a version has the figures of a row. No Claude model has a
	// row: its figures come from Anthropic's model list.
	for _, model := range []string{"gpt-5.6-sol", "claude-opus-6", "glm-5", "gpt-latest"} {
		_, _, err := figures(model)
		var unknown *models.MetadataError
		require.ErrorAs(t, err, &unknown, model)
	}

	// A configured figure outranks the guess, and the other is still guessed.
	set, err := models.Select([]string{"codex:work/gpt-7-sol"}, models.Unlisted{Configured: models.Metadata{ContextWindow: 300_000}}, "")
	require.NoError(t, err)
	require.Equal(t, models.Metadata{ContextWindow: 300_000, MaxOutputTokens: newest.MaxOutputTokens}, set.Entries[0].Metadata)
	require.Equal(t, "gpt-6.1-sol", set.Entries[0].GuessedFrom)
}

// A provider's model list gives the figures of a model the table does not
// have, and is kept in a file between starts.
func TestSelectTakesAFigureFromTheProvidersList(t *testing.T) {
	// What Anthropic's list gave on 2026-11-12, and what the Codex backend's
	// gave, which has no output limit, for a model the table has and for
	// one it does not.
	lists := models.Lists{
		"claude:work": {FetchedAtUnixMs: 1794504428000, Models: map[string]models.Listed{
			"claude-opus-4-5-20251101": {ContextWindow: 200_000, MaxOutputTokens: 64_000},
			"claude-opus-5-5":          {ContextWindow: 1_000_000, MaxOutputTokens: 128_000},
		}},
		"codex:chatgpt": {FetchedAtUnixMs: 1794504428000, Models: map[string]models.Listed{
			"gpt-6.1-sol": {ContextWindow: 500_000},
			"gpt-5.6-sol": {ContextWindow: 272_000},
		}},
	}
	path := filepath.Join(t.TempDir(), "state", "models.json")
	none, err := models.LoadLists(path)
	require.NoError(t, err)
	require.Empty(t, none)
	require.NoError(t, lists.Save(path))
	kept, err := models.LoadLists(path)
	require.NoError(t, err)
	require.Equal(t, lists, kept)

	entry := func(unlisted models.Unlisted, name string) (models.Entry, error) {
		m, err := models.Select([]string{name}, unlisted, "")
		if err != nil {
			return models.Entry{}, err
		}
		return m.Entries[0], nil
	}
	fromLists := models.Unlisted{Lists: kept}

	// Without its account's list nothing is known about a Claude model.
	_, err = entry(models.Unlisted{}, "claude:work/claude-opus-5-5")
	var unknown *models.MetadataError
	require.ErrorAs(t, err, &unknown)
	current, err := entry(fromLists, "claude:work/claude-opus-5-5")
	require.NoError(t, err)
	require.Equal(t, models.Metadata{ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, current.Metadata)
	// The list names an older model with its date.
	old, err := entry(fromLists, "claude:work/claude-opus-4-5")
	require.NoError(t, err)
	require.Equal(t, models.Metadata{ContextWindow: 200_000, MaxOutputTokens: 64_000}, old.Metadata)
	require.True(t, old.Listed)
	require.Empty(t, old.GuessedFrom)

	// The list is the account's: another account's says nothing.
	_, err = entry(fromLists, "claude:other/claude-opus-4-5")
	require.ErrorAs(t, err, &unknown)

	// The table outranks the list for a model it has.
	tabled, err := entry(fromLists, "codex:chatgpt/gpt-6.1-sol")
	require.NoError(t, err)
	require.EqualValues(t, 1_050_000, tabled.Metadata.ContextWindow)
	require.False(t, tabled.Listed)

	// A figure the list does not give is guessed, and one nothing gives is
	// asked for by its key.
	_, err = entry(fromLists, "codex:chatgpt/gpt-5.6-sol")
	require.ErrorAs(t, err, &unknown)
	require.Equal(t, []string{config.KeyModelMaxOutputTokens}, unknown.Missing)
	fromLists.Configured.MaxOutputTokens = 100_000
	both, err := entry(fromLists, "codex:chatgpt/gpt-5.6-sol")
	require.NoError(t, err)
	require.Equal(t, models.Metadata{ContextWindow: 272_000, MaxOutputTokens: 100_000}, both.Metadata)
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
	unlisted := models.Unlisted{Configured: models.Metadata{ContextWindow: cfg.ModelContextWindow, MaxOutputTokens: cfg.ModelMaxOutputTokens}}

	first, err := models.Select(cfg.Models, unlisted, "")
	require.NoError(t, err)
	require.Equal(t, "claude-opus-4-7", first.Entries[0].Model)
	require.Equal(t, "medium", first.Entries[0].Effort)
	require.Equal(t, unlisted.Configured, first.Entries[1].Metadata)

	// A flag lists the models separated by commas, and replaces the file's.
	loaded, err = config.Load(config.Sources{Files: []string{path}}, "--models", "zai:main/glm-5#high,claude:work/claude-opus-4-7")
	require.NoError(t, err)
	require.Equal(t, []string{"zai:main/glm-5#high", "claude:work/claude-opus-4-7"}, loaded.Config.Models)
}
