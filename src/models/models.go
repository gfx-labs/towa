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

// Unlisted is where the figures of a model the built-in table does not have
// come from. A figure is taken from the first that gives it: the
// configuration, the provider's model list, then the row the model's name
// points to (see guess).
type Unlisted struct {
	// Configured are the configuration's figures, which are those of every
	// such model. A zero figure is one that is not set.
	Configured Metadata
	// Lists are the model lists read from providers.
	Lists Lists
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

// UnknownModelError reports a name that no configured model has.
type UnknownModelError struct{ Name string }

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("models: %q is not a configured model", e.Name)
}

// MetadataError reports an entry whose model is not in the built-in table
// and whose missing figures the configuration does not give.
type MetadataError struct {
	// Entry is the entry as the configuration writes it.
	Entry string
	// Missing are the config keys that would complete it.
	Missing []string
}

func (e *MetadataError) Error() string {
	return fmt.Sprintf("models: nothing is known about %q: set %s", e.Entry, strings.Join(e.Missing, " and "))
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
// empty name selects the first. The model's chain is the chosen entry and
// every entry listed after it. A model the built-in table does not have
// takes its figures from unlisted. The table outranks a provider's list:
// Anthropic's lists 1M for claude-opus-4-6, which has it only with a beta
// this client does not send.
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
	at := slices.Index(configured, name)
	if at < 0 {
		return Model{}, &UnknownModelError{Name: name}
	}
	m := Model{Name: name}
	for _, raw := range configured[at:] {
		parsed, err := config.ParseChainEntry(raw)
		if err != nil {
			return Model{}, fmt.Errorf("models: %w", err)
		}
		meta, listed := builtin[parsed.Model]
		var guessedFrom string
		var fromList bool
		if !listed {
			meta = unlisted.Configured
			list, _ := unlisted.Lists.Lookup(parsed)
			guessed, row, _ := guess(parsed.Model)
			for _, figure := range []struct {
				to              *int64
				listed, guessed int64
			}{
				{&meta.ContextWindow, list.ContextWindow, guessed.ContextWindow},
				{&meta.MaxOutputTokens, list.MaxOutputTokens, guessed.MaxOutputTokens},
			} {
				switch {
				case *figure.to != 0:
				case figure.listed != 0:
					*figure.to, fromList = figure.listed, true
				case figure.guessed != 0:
					*figure.to, guessedFrom = figure.guessed, row
				}
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
			return Model{}, &MetadataError{Entry: raw, Missing: missing}
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
