package config

import (
	"fmt"
	"slices"
	"strings"
)

// Providers that need no providers entry: the harness has their
// endpoints and their OAuth flows built in.
const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
)

// ChainEntry is one parsed entry of the models list.
type ChainEntry struct {
	// Provider is a built-in provider or the id of a configured one.
	Provider string
	// Account is the id of a configured account.
	Account string
	// Model is the provider's own name for the model. It may hold "/" and
	// ":", as OpenRouter's names do.
	Model string
	// Effort is the reasoning effort sent with a request to this entry, in
	// the provider's own words, such as "medium". It is not checked: a
	// word the provider does not take is the provider's error. Empty sends
	// none, which leaves the provider's default.
	Effort string
}

// ParseChainEntry parses "provider:account/model" or
// "provider:account/model#effort". The provider ends at the first ":", the
// account at the first "/" after it, and the model at the last "#".
func ParseChainEntry(entry string) (ChainEntry, error) {
	bad := fmt.Errorf("%q is not provider:account/model or provider:account/model#effort", entry)
	target, effort := entry, ""
	if i := strings.LastIndex(entry, "#"); i >= 0 {
		target, effort = entry[:i], entry[i+1:]
		if effort == "" {
			return ChainEntry{}, bad
		}
	}
	provider, rest, ok := strings.Cut(target, ":")
	account, model, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || provider == "" || account == "" || model == "" || strings.Contains(provider, "/") {
		return ChainEntry{}, bad
	}
	return ChainEntry{Provider: provider, Account: account, Model: model, Effort: effort}, nil
}

// validateModels reports entries the program cannot act on. No model at all
// is valid: the process then has no model to run a turn with, which is for
// the turn to report.
func validateModels(models []string, accounts []Account, providers []Provider) error {
	accountProvider := map[string]string{}
	for _, a := range accounts {
		accountProvider[a.ID] = a.Provider
	}
	known := map[string]bool{ProviderClaude: true, ProviderCodex: true}
	for _, p := range providers {
		known[p.ID] = true
	}
	for i, raw := range models {
		at := fmt.Sprintf("models[%d]", i)
		entry, err := ParseChainEntry(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", at, err)
		}
		// A session selects a model by its text, so two of one text could
		// not be told apart.
		if slices.Contains(models[:i], raw) {
			return fmt.Errorf("%s: %q is listed twice", at, raw)
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
	return nil
}
