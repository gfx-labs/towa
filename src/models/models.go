// Package models turns the configured models into what a turn needs: the
// chain of router entries a request goes down, and each entry's metadata
// and the model's tool dialect.
package models

import (
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/provider/router"
)

// Families the dialect rule knows. A family is otherwise any text.
const (
	FamilyClaude = "claude"
	FamilyGPT    = "gpt"
	FamilyCodex  = "codex"
)

// Metadata is what is known about one chain entry's model.
type Metadata struct {
	// ContextWindow is the context size in tokens.
	ContextWindow int64
	// MaxOutputTokens is the most tokens one response may hold.
	MaxOutputTokens int64
	// Family names what the model is.
	Family string
}

// Entry is one parsed chain entry with its metadata.
type Entry struct {
	config.ChainEntry
	Metadata Metadata
}

// Model is a configured model, resolved.
type Model struct {
	// Name is the model's name in the configuration.
	Name string
	// Entries are the chain's entries in order. A fallback can move a
	// request to an entry with a smaller context window, so each has its
	// own metadata.
	Entries []Entry
	// Dialect is the tool dialect, one of the config.Dialect values. It is
	// one value for the whole chain, because a fallback happens inside a
	// turn and the tools cannot change there.
	Dialect string
}

// NoModelError reports a selection when no model is configured.
type NoModelError struct{}

func (*NoModelError) Error() string { return "models: no model is configured" }

// UnknownModelError reports a name that no configured model has.
type UnknownModelError struct{ Name string }

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("models: %q is not a configured model", e.Name)
}

// MetadataError reports a chain entry whose model is not in the built-in
// table and whose missing figures the configuration does not give.
type MetadataError struct {
	// Name is the configured model and Entry the chain entry.
	Name  string
	Entry string
	// Missing are the config keys, under models.<name>, that would
	// complete it.
	Missing []string
}

func (e *MetadataError) Error() string {
	return fmt.Sprintf("models: %s: nothing is known about %q: set models.%s.%s",
		e.Name, e.Entry, e.Name, strings.Join(e.Missing, " and models."+e.Name+"."))
}

// DialectError reports a chain whose entries resolve to different tool
// dialects and no configured dialect to settle it.
type DialectError struct {
	Name string
	// Dialects are the dialects of the entries, in chain order.
	Dialects []string
}

func (e *DialectError) Error() string {
	return fmt.Sprintf("models: %s: the chain's entries use different tool dialects %q: set models.%s.dialect",
		e.Name, e.Dialects, e.Name)
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

// Select resolves the model a session chose. An empty name selects the
// default. It is called when a model is selected, so that a model nothing
// is known about fails then and not in the middle of a turn.
func Select(defaultModel string, configured map[string]config.Model, name string) (Model, error) {
	if len(configured) == 0 {
		return Model{}, &NoModelError{}
	}
	if name == "" {
		name = defaultModel
	}
	cfg, ok := configured[name]
	if !ok {
		return Model{}, &UnknownModelError{Name: name}
	}
	return Resolve(name, cfg)
}

// Resolve parses a model's chain and resolves each entry's metadata and
// the model's dialect. A figure in the configuration replaces the built-in
// one for every entry of the chain.
func Resolve(name string, cfg config.Model) (Model, error) {
	m := Model{Name: name, Dialect: cfg.Dialect}
	var dialects []string
	mixed := false
	for _, raw := range cfg.Chain {
		parsed, err := config.ParseChainEntry(raw)
		if err != nil {
			return Model{}, fmt.Errorf("models: %s: %w", name, err)
		}
		meta := builtin[parsed.Model]
		if cfg.ContextWindow != 0 {
			meta.ContextWindow = cfg.ContextWindow
		}
		if cfg.MaxOutputTokens != 0 {
			meta.MaxOutputTokens = cfg.MaxOutputTokens
		}
		if cfg.Family != "" {
			meta.Family = cfg.Family
		}
		var missing []string
		if meta.ContextWindow == 0 {
			missing = append(missing, "context_window")
		}
		if meta.MaxOutputTokens == 0 {
			missing = append(missing, "max_output_tokens")
		}
		// The family is needed only to choose a dialect.
		if meta.Family == "" && cfg.Dialect == "" {
			missing = append(missing, "family")
		}
		if len(missing) > 0 {
			return Model{}, &MetadataError{Name: name, Entry: raw, Missing: missing}
		}
		m.Entries = append(m.Entries, Entry{ChainEntry: parsed, Metadata: meta})
		dialect := DialectOf(meta.Family)
		mixed = mixed || (len(dialects) > 0 && dialect != dialects[0])
		dialects = append(dialects, dialect)
	}
	if len(m.Entries) == 0 {
		return Model{}, fmt.Errorf("models: %s: the chain is empty", name)
	}
	if m.Dialect == "" {
		if mixed {
			return Model{}, &DialectError{Name: name, Dialects: dialects}
		}
		m.Dialect = dialects[0]
	}
	return m, nil
}

// DialectOf returns the tool dialect of a model family, as the design's
// dialect table gives it: Claude models use claude, GPT and Codex models
// use codex, and any other family uses generic. The table's "distilled
// from" case has no entry here, because no source for one was read: such a
// model gets its dialect from models.<name>.dialect.
func DialectOf(family string) string {
	switch family {
	case FamilyClaude:
		return config.DialectClaude
	case FamilyGPT, FamilyCodex:
		return config.DialectCodex
	}
	return config.DialectGeneric
}

// Chain returns the model's entries as the router takes them. clients
// returns the client for a provider id.
func (m Model) Chain(clients func(providerID string) (provider.Provider, bool)) ([]router.Entry, error) {
	chain := make([]router.Entry, len(m.Entries))
	for i, e := range m.Entries {
		client, ok := clients(e.Provider)
		if !ok || client == nil {
			entry := router.Entry{ProviderID: e.Provider, Account: e.Account, Model: e.Model}
			return nil, &ProviderError{Name: m.Name, Entry: entry.String(), Provider: e.Provider}
		}
		chain[i] = router.Entry{Provider: client, ProviderID: e.Provider, Account: e.Account, Model: e.Model}
	}
	return chain, nil
}
