package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Auth configures how logins are made and kept fresh. The defaults are
// jcode's: its freshness margin, its Claude token request timeout and its
// longer callback wait, the Codex one.
type Auth struct {
	// RefreshMarginSeconds is how long before its expiry an OAuth token is
	// refreshed, so that a request does not leave with a token that expires
	// on the way.
	RefreshMarginSeconds int `name:"refresh_margin_seconds" default:"60" validate:"gte=0" help:"Seconds before its expiry that an OAuth token is refreshed."`
	// TokenTimeoutSeconds bounds one request to an OAuth token endpoint.
	TokenTimeoutSeconds int `name:"token_timeout_seconds" default:"15" validate:"gt=0" help:"Seconds one request to an OAuth token endpoint may take."`
	// LoginTimeoutSeconds is how long login waits for the browser to come
	// back before it asks for the code to be pasted.
	LoginTimeoutSeconds int `name:"login_timeout_seconds" default:"300" validate:"gt=0" help:"Seconds login waits for the browser to come back before it asks for the code."`
}

func (a Auth) validate() error {
	return validateFields("auth", a)
}

// Account is one login or key the harness can send requests with.
type Account struct {
	// ID names the account in model chains and in the auth store.
	ID string `json:"id" validate:"required"`
	// Provider is the provider the account belongs to, such as "claude",
	// "codex" or "openrouter".
	Provider string `json:"provider" validate:"required"`
	Kind     string `json:"kind" validate:"oneof=oauth api_key proxied_oauth proxied_api_key none"`
	// APIKeyEnv names the environment variable that holds the key of an
	// api_key account.
	APIKeyEnv string `json:"api_key_env" validate:"required_if=Kind api_key,excluded_unless=Kind api_key"`
	// ChatGPTAccountID is the account id the Codex backend wants in a
	// header. Only a proxied_oauth account sets it: the proxy replaces the
	// authorization and nothing else, and the harness has no id token to
	// read the id from.
	ChatGPTAccountID string `json:"chatgpt_account_id" validate:"excluded_unless=Kind proxied_oauth"`
	// Stored says no config layer declares the account: it is one of the
	// auth store, made by a login or named by a model chain ahead of it.
	Stored bool `json:"-"`
}

// Kinds of an account.
const (
	// AccountOAuth: the harness holds the token and refreshes it.
	AccountOAuth = "oauth"
	// AccountAPIKey: the key is read from the environment.
	AccountAPIKey = "api_key"
	// AccountProxiedOAuth and AccountProxiedAPIKey: a proxy holds the
	// credential. The harness sends a placeholder and builds the rest of the
	// request as for the kind it stands for.
	AccountProxiedOAuth  = "proxied_oauth"
	AccountProxiedAPIKey = "proxied_api_key"
	// AccountNone: the endpoint takes no credential, as a model server on
	// the local network does. No authorization is sent.
	AccountNone = "none"
)

// addStoredAccounts adds the accounts of the auth store, given as the
// provider of each by id, and then the accounts that model chains name on a
// built-in provider. A login makes such an account, so a chain may name it
// before the login exists, and its use reports that it is not logged in.
// An id that a config layer declares keeps the declared account.
func (c *Config) addStoredAccounts(stored map[string]string) {
	known := map[string]bool{}
	for _, a := range c.Accounts {
		known[a.ID] = true
	}
	add := func(id, provider string) {
		if !known[id] {
			known[id] = true
			c.Accounts = append(c.Accounts, Account{ID: id, Provider: provider, Kind: AccountOAuth, Stored: true})
		}
	}
	// In id and name order, so that the same input always gives the same list.
	for _, id := range slices.Sorted(maps.Keys(stored)) {
		add(id, stored[id])
	}
	for _, name := range slices.Sorted(maps.Keys(c.Models)) {
		for _, raw := range c.Models[name].Chain {
			// An entry that does not parse is for validation to report.
			entry, err := ParseChainEntry(raw)
			if err == nil && (entry.Provider == ProviderClaude || entry.Provider == ProviderCodex) {
				add(entry.Account, entry.Provider)
			}
		}
	}
}

func (c *Config) validateAccounts() error {
	seen := map[string]bool{}
	for i, a := range c.Accounts {
		at := fmt.Sprintf("accounts[%d]", i)
		if a.Stored {
			at = fmt.Sprintf("auth store account %q", a.ID)
		}
		if err := validateFields(at, a); err != nil {
			return err
		}
		switch {
		case a.ID != filepath.Base(a.ID) || strings.HasPrefix(a.ID, "."):
			// The id names the account's file in the auth store.
			return fmt.Errorf("%s.id: %q is not a plain file name", at, a.ID)
		case seen[a.ID]:
			return fmt.Errorf("%s.id: %q is used by more than one account", at, a.ID)
		case a.Kind == AccountNone && (a.Provider == ProviderClaude || a.Provider == ProviderCodex):
			// Both built-in providers refuse a request without a credential.
			return fmt.Errorf("%s.kind: %q is for a configured provider, and %q always takes a credential", at, a.Kind, a.Provider)
		}
		seen[a.ID] = true
	}
	return nil
}
