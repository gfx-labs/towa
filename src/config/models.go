package config

import (
	"fmt"
	"slices"
	"strings"
)

// Model is one named model: the chain of provider entries a request goes
// down, and what is known about the model when the built-in table in
// src/models does not know it or is wrong for this endpoint.
type Model struct {
	// Chain is tried in order. Each entry is "provider:account/model".
	Chain []string `koanf:"chain"`
	// ContextWindow is the model's context size in tokens. Zero takes the
	// built-in figure.
	ContextWindow int64 `koanf:"context_window"`
	// MaxOutputTokens is the most tokens one response may hold. Zero takes
	// the built-in figure.
	MaxOutputTokens int64 `koanf:"max_output_tokens"`
	// Family names what the model is, such as "claude" or "gpt". It decides
	// the tool dialect when Dialect is empty.
	Family string `koanf:"family"`
	// Dialect pins the tool dialect: one of the Dialect values.
	Dialect string `koanf:"dialect"`
}

// Tool dialects.
const (
	DialectClaude  = "claude"
	DialectCodex   = "codex"
	DialectGeneric = "generic"
)

// Providers that need no [[providers]] entry: the harness has their
// endpoints and their OAuth flows built in.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// ChainEntry is one parsed entry of a model's chain.
type ChainEntry struct {
	// Provider is a built-in provider or the id of a configured one.
	Provider string
	// Account is the id of a configured account.
	Account string
	// Model is the provider's own name for the model. It may hold "/" and
	// ":", as OpenRouter's names do.
	Model string
}

// ParseChainEntry parses "provider:account/model". The provider ends at the
// first ":" and the account at the first "/" after it.
func ParseChainEntry(entry string) (ChainEntry, error) {
	provider, rest, ok := strings.Cut(entry, ":")
	account, model, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || provider == "" || account == "" || model == "" || strings.Contains(provider, "/") {
		return ChainEntry{}, fmt.Errorf("%q is not provider:account/model", entry)
	}
	return ChainEntry{Provider: provider, Account: account, Model: model}, nil
}

// validateModelSet reports models the program cannot act on. No model at
// all is valid: the process then has no model to run a turn with, which is
// for the turn to report.
func validateModelSet(defaultModel string, models map[string]Model, accounts []Account, providers []Provider) error {
	if len(models) == 0 {
		if defaultModel != "" {
			return fmt.Errorf("default_model: %q names no model, and no model is configured", defaultModel)
		}
		return nil
	}
	if _, ok := models[defaultModel]; !ok {
		if defaultModel == "" {
			return fmt.Errorf("default_model is empty and models are configured")
		}
		return fmt.Errorf("default_model: %q names no model", defaultModel)
	}
	accountProvider := map[string]string{}
	for _, a := range accounts {
		accountProvider[a.ID] = a.Provider
	}
	known := map[string]bool{ProviderClaude: true, ProviderCodex: true}
	for _, p := range providers {
		known[p.ID] = true
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	// In name order, so that the same config always reports the same error.
	slices.Sort(names)
	for _, name := range names {
		m, at := models[name], "models."+name
		switch {
		case name == "":
			return fmt.Errorf("models: a model has an empty name")
		case len(m.Chain) == 0:
			return fmt.Errorf("%s.chain is empty", at)
		case m.ContextWindow < 0:
			return fmt.Errorf("%s.context_window: %d is negative", at, m.ContextWindow)
		case m.MaxOutputTokens < 0:
			return fmt.Errorf("%s.max_output_tokens: %d is negative", at, m.MaxOutputTokens)
		}
		switch m.Dialect {
		case "", DialectClaude, DialectCodex, DialectGeneric:
		default:
			return fmt.Errorf("%s.dialect: %q is not %q, %q or %q", at, m.Dialect, DialectClaude, DialectCodex, DialectGeneric)
		}
		for i, raw := range m.Chain {
			at := fmt.Sprintf("%s.chain[%d]", at, i)
			entry, err := ParseChainEntry(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			provider, ok := accountProvider[entry.Account]
			switch {
			case !known[entry.Provider]:
				return fmt.Errorf("%s: provider %q is not built in (%q, %q) and not configured", at, entry.Provider, ProviderClaude, ProviderCodex)
			case !ok:
				return fmt.Errorf("%s: account %q is not configured", at, entry.Account)
			case provider != entry.Provider:
				return fmt.Errorf("%s: account %q belongs to provider %q, not %q", at, entry.Account, provider, entry.Provider)
			}
		}
	}
	return nil
}
