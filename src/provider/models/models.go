// Package models turns the configured models into what a turn needs: the
// chain of router entries a request goes down and each entry's metadata.
package models

import (
	"fmt"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/router"
)

// Metadata is what is known about one chain entry's model.
type Metadata struct {
	// ContextWindow is the context size in tokens.
	ContextWindow int64
	// MaxOutputTokens is the most tokens one response may hold.
	MaxOutputTokens int64
	// NoAdaptiveThinking says the model refuses the adaptive thinking
	// parameter.
	NoAdaptiveThinking bool
}

// Entry is one parsed chain entry with its metadata.
type Entry struct {
	config.ChainEntry
	Metadata Metadata
	// Listed says a figure came from the provider's model list.
	Listed bool
	// GuessedFrom is the table row a figure was taken from, when the model
	// has no row of its own and its name pointed to one.
	GuessedFrom string
}

// Unlisted is where a model's figures come from other than the built-in
// table. A figure is taken from the first that gives it: the models of the
// entry's provider, the table, the configuration's two keys, the provider's
// model list, then the row the model's name points to (see guess).
type Unlisted struct {
	// Providers are the configured providers. One that describes a model
	// says what that model is on that endpoint, so it outranks the table.
	Providers []config.Provider
	// Configured are the configuration's figures, which are those of every
	// model the table does not have. A zero figure is one that is not set.
	Configured Metadata
	// Lists are the model lists read from providers.
	Lists Lists
	// Accounts are the configured accounts. An entry whose account is
	// config.AnyAccount stands for one entry per account of its provider.
	Accounts []config.Account
}

// Model is a selected model, resolved.
type Model struct {
	// Name is the selected entry as the configuration writes it.
	Name string
	// Entries are the selected entry and the ones listed after it, in
	// order. A fallback can move a request to an entry with a smaller
	// context window or another effort, so each has its own.
	Entries []Entry
}

// NoModelError reports a selection when no model is configured.
type NoModelError struct{}

func (*NoModelError) Error() string { return "models: no model is configured" }

// NoAccountError reports an entry for every account of a provider that
// has none.
type NoAccountError struct{ Entry string }

func (e *NoAccountError) Error() string {
	return fmt.Sprintf("models: %q: provider has no account: log in or configure one", e.Entry)
}

// MetadataError reports an entry whose model is not in the built-in table
// and whose missing figures the configuration does not give.
type MetadataError struct {
	// Entry is the entry as the configuration writes it.
	Entry string
	// Missing are the config keys that would complete it, for every model
	// the table does not have.
	Missing []string
	// Provider is the entry's provider when it is a configured one, whose
	// models can give the figures of this model alone.
	Provider string
}

func (e *MetadataError) Error() string {
	keys := strings.Join(e.Missing, " and ")
	if e.Provider == "" {
		return fmt.Sprintf("models: nothing is known about %q: set %s", e.Entry, keys)
	}
	return fmt.Sprintf("models: nothing is known about %q: describe it in the models of provider %q, or set %s", e.Entry, e.Provider, keys)
}

// ProviderError reports a chain entry whose provider has no client.
type ProviderError struct {
	Name     string
	Entry    string
	Provider string
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("models: %s: %q: there is no client for provider %q", e.Name, e.Entry, e.Provider)
}

// Select resolves the model a session chose from the configured list. An
// empty name selects the first, and a short name the entry
// config.ResolveModel gives. The model's chain is the chosen entry and
// every entry listed after it, with an entry for every account expanded to
// one per account. A figure that the entry's provider gives for
// the model in its configuration is the one used. Otherwise a model the
// built-in table has takes the table's, and any other takes its figures
// from the rest of unlisted. The table outranks a provider's list: it holds
// only the models a list does not settle.
//
// It is called when a model is selected, so that a model nothing is known
// about fails then and not in the middle of a turn.
func Select(configured []string, unlisted Unlisted, name string) (Model, error) {
	if len(configured) == 0 {
		return Model{}, &NoModelError{}
	}
	if name == "" {
		name = configured[0]
	}
	name, err := config.ResolveModel(configured, name)
	if err != nil {
		return Model{}, fmt.Errorf("models: %w", err)
	}
	var chain []config.ChainEntry
	for _, raw := range configured[slices.Index(configured, name):] {
		parsed, err := config.ParseChainEntry(raw)
		if err != nil {
			return Model{}, fmt.Errorf("models: %w", err)
		}
		expanded := config.ExpandAccounts(parsed, unlisted.Accounts)
		if len(expanded) == 0 {
			return Model{}, &NoAccountError{Entry: raw}
		}
		chain = append(chain, expanded...)
	}
	m := Model{Name: name}
	for _, parsed := range chain {
		raw := parsed.String()
		meta, tabled := builtin[parsed.Model]
		var configuredProvider string
		var described config.ProviderModel
		for _, p := range unlisted.Providers {
			if p.ID != parsed.Provider {
				continue
			}
			configuredProvider = p.ID
			if i := slices.IndexFunc(p.Models, func(m config.ProviderModel) bool { return m.ID == parsed.Model }); i >= 0 {
				described = p.Models[i]
			}
		}
		var list Listed
		var guessed Metadata
		var row, guessedFrom string
		var fromList bool
		if !tabled {
			meta = unlisted.Configured
			list, _ = unlisted.Lists.Lookup(parsed)
			guessed, row, _ = guess(parsed.Model)
		}
		for _, figure := range []struct {
			to                         *int64
			described, listed, guessed int64
		}{
			{&meta.ContextWindow, described.ContextWindow, list.ContextWindow, guessed.ContextWindow},
			{&meta.MaxOutputTokens, described.MaxOutputTokens, list.MaxOutputTokens, guessed.MaxOutputTokens},
		} {
			switch {
			case figure.described != 0:
				*figure.to = figure.described
			case *figure.to != 0:
			case figure.listed != 0:
				*figure.to, fromList = figure.listed, true
			case figure.guessed != 0:
				*figure.to, guessedFrom = figure.guessed, row
			}
		}
		var missing []string
		if meta.ContextWindow == 0 {
			missing = append(missing, config.KeyModelContextWindow)
		}
		if meta.MaxOutputTokens == 0 {
			missing = append(missing, config.KeyModelMaxOutputTokens)
		}
		if len(missing) > 0 {
			return Model{}, &MetadataError{Entry: raw, Missing: missing, Provider: configuredProvider}
		}
		m.Entries = append(m.Entries, Entry{ChainEntry: parsed, Metadata: meta, Listed: fromList, GuessedFrom: guessedFrom})
	}
	return m, nil
}

// Chain returns the model's entries as the router takes them. clients
// returns the client for a provider id.
func (m Model) Chain(clients func(providerID string) (provider.Provider, bool)) ([]router.Entry, error) {
	chain := make([]router.Entry, len(m.Entries))
	for i, e := range m.Entries {
		entry := router.Entry{ProviderID: e.Provider, Account: e.Account, Model: e.Model, Effort: e.Effort}
		client, ok := clients(e.Provider)
		if !ok || client == nil {
			return nil, &ProviderError{Name: m.Name, Entry: entry.String(), Provider: e.Provider}
		}
		entry.Provider = client
		chain[i] = entry
	}
	return chain, nil
}
