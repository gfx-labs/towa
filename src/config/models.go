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
	// Account is the id of a configured account, or AnyAccount.
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

// AnyAccount in place of an account makes an entry stand for one entry per
// account of its provider, in the order of the accounts list. "provider:model"
// is short for "provider:*/model".
const AnyAccount = "*"

// String returns the entry as the models list writes it.
func (e ChainEntry) String() string {
	s := e.Provider + ":" + e.Account + "/" + e.Model
	if e.Effort != "" {
		s += "#" + e.Effort
	}
	return s
}

// ExpandAccounts returns the entry once for each account of its provider
// when its account is AnyAccount, and the entry alone otherwise.
func ExpandAccounts(e ChainEntry, accounts []Account) []ChainEntry {
	if e.Account != AnyAccount {
		return []ChainEntry{e}
	}
	var out []ChainEntry
	for _, a := range accounts {
		if a.Provider == e.Provider {
			one := e
			one.Account = a.ID
			out = append(out, one)
		}
	}
	return out
}

// ParseChainEntry parses "provider:account/model" or "provider:model", each
// optionally with "#effort". The provider ends at the first ":", the account
// at the first "/" after it, and the model at the last "#". Without a "/" the
// account is AnyAccount. A model name that holds "/", as a gateway's does,
// needs the account written out.
func ParseChainEntry(entry string) (ChainEntry, error) {
	bad := fmt.Errorf("%q is not provider:account/model or provider:model, optionally with #effort", entry)
	target, effort := entry, ""
	if i := strings.LastIndex(entry, "#"); i >= 0 {
		target, effort = entry[:i], entry[i+1:]
		if effort == "" {
			return ChainEntry{}, bad
		}
	}
	provider, rest, ok := strings.Cut(target, ":")
	account, model, hasAccount := strings.Cut(rest, "/")
	if !hasAccount {
		account, model = AnyAccount, rest
	}
	if !ok || provider == "" || account == "" || model == "" || strings.Contains(provider, "/") {
		return ChainEntry{}, bad
	}
	return ChainEntry{Provider: provider, Account: account, Model: model, Effort: effort}, nil
}

// AmbiguousModelError reports a name that matches more than one entry in a
// way that changes what is sent.
type AmbiguousModelError struct {
	Name    string
	Matches []string
}

func (e *AmbiguousModelError) Error() string {
	return fmt.Sprintf("%q matches more than one model: %s", e.Name, strings.Join(e.Matches, ", "))
}

// UnknownModelError reports a name that no entry matches.
type UnknownModelError struct{ Name string }

func (e *UnknownModelError) Error() string {
	return fmt.Sprintf("%q is not a configured model", e.Name)
}

// ResolveModel returns the entry of configured that name selects. name is
// an entry as written, or a short form of one: "model", "model#effort",
// "provider:model" or "provider:model#effort". A short form leaves out the
// account, so it means every account, and selects the first entry it
// matches when the matches differ only in their account: the entries after
// it are its fallbacks. Matches that differ in provider or effort are
// ambiguous and refused.
func ResolveModel(configured []string, name string) (string, error) {
	if slices.Contains(configured, name) {
		return name, nil
	}
	var entries []ChainEntry
	for _, raw := range configured {
		e, err := ParseChainEntry(raw)
		if err != nil {
			return "", err
		}
		entries = append(entries, e)
	}
	want := ChainEntry{Model: name}
	if i := strings.LastIndex(want.Model, "#"); i >= 0 {
		want.Model, want.Effort = want.Model[:i], want.Model[i+1:]
	}
	// "provider:model" when provider is one the list uses. Otherwise the
	// ":" is the model's own, as in llama3.2:3b.
	if parsed, err := ParseChainEntry(name); err == nil && parsed.Account == AnyAccount &&
		slices.ContainsFunc(entries, func(e ChainEntry) bool { return e.Provider == parsed.Provider }) {
		want = parsed
	}
	var matches []string
	var first ChainEntry
	ambiguous := false
	for i, e := range entries {
		if e.Model != want.Model || want.Provider != "" && e.Provider != want.Provider || want.Effort != "" && e.Effort != want.Effort {
			continue
		}
		if len(matches) == 0 {
			first = e
		}
		ambiguous = ambiguous || e.Provider != first.Provider || e.Effort != first.Effort
		matches = append(matches, configured[i])
	}
	switch {
	case len(matches) == 0:
		return "", &UnknownModelError{Name: name}
	case ambiguous:
		return "", &AmbiguousModelError{Name: name, Matches: matches}
	}
	return matches[0], nil
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
		case entry.Account == AnyAccount:
			// Expanded when the model is selected. A provider with no
			// account is reported then, as one not logged in is.
		case !ok:
			return fmt.Errorf("%s: account %q is not configured", at, entry.Account)
		case provider != entry.Provider:
			return fmt.Errorf("%s: account %q belongs to provider %q, not %q", at, entry.Account, provider, entry.Provider)
		}
	}
	return nil
}
