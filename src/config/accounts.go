package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Auth configures how logins are made and kept fresh.
type Auth struct {
	// RefreshMarginSeconds is how long before its expiry an OAuth token is
	// refreshed, so that a request does not leave with a token that expires
	// on the way.
	RefreshMarginSeconds int `koanf:"refresh_margin_seconds"`
	// TokenTimeoutSeconds bounds one request to an OAuth token endpoint.
	TokenTimeoutSeconds int `koanf:"token_timeout_seconds"`
	// LoginTimeoutSeconds is how long login waits for the browser to come
	// back before it asks for the code to be pasted.
	LoginTimeoutSeconds int `koanf:"login_timeout_seconds"`
}

// The values are jcode's: its freshness margin, its Claude token request
// timeout and its longer callback wait, the Codex one.
var authDefaults = map[string]any{
	"auth.refresh_margin_seconds": 60,
	"auth.token_timeout_seconds":  15,
	"auth.login_timeout_seconds":  300,
}

func (a Auth) validate() error {
	switch {
	case a.RefreshMarginSeconds < 0:
		return fmt.Errorf("auth.refresh_margin_seconds: %d is negative", a.RefreshMarginSeconds)
	case a.TokenTimeoutSeconds <= 0:
		return fmt.Errorf("auth.token_timeout_seconds: %d is not positive", a.TokenTimeoutSeconds)
	case a.LoginTimeoutSeconds <= 0:
		return fmt.Errorf("auth.login_timeout_seconds: %d is not positive", a.LoginTimeoutSeconds)
	}
	return nil
}

// Account is one login or key the harness can send requests with.
type Account struct {
	// ID names the account in model chains and in the auth store.
	ID string `koanf:"id"`
	// Provider is the provider the account belongs to, such as "claude",
	// "codex" or "openrouter".
	Provider string `koanf:"provider"`
	Kind     string `koanf:"kind"`
	// APIKeyEnv names the environment variable that holds the key of an
	// api_key account.
	APIKeyEnv string `koanf:"api_key_env"`
	// ChatGPTAccountID is the account id the Codex backend wants in a
	// header. Only a proxied_oauth account sets it: the proxy replaces the
	// authorization and nothing else, and the harness has no id token to
	// read the id from.
	ChatGPTAccountID string `koanf:"chatgpt_account_id"`
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
)

func (c *Config) validateAccounts() error {
	seen := map[string]bool{}
	for i, a := range c.Accounts {
		at := fmt.Sprintf("accounts[%d]", i)
		switch {
		case a.ID == "":
			return fmt.Errorf("%s.id is empty", at)
		case a.ID != filepath.Base(a.ID) || strings.HasPrefix(a.ID, "."):
			// The id names the account's file in the auth store.
			return fmt.Errorf("%s.id: %q is not a plain file name", at, a.ID)
		case seen[a.ID]:
			return fmt.Errorf("%s.id: %q is used by more than one account", at, a.ID)
		case a.Provider == "":
			return fmt.Errorf("%s.provider is empty", at)
		}
		seen[a.ID] = true
		switch a.Kind {
		case AccountAPIKey:
			if a.APIKeyEnv == "" {
				return fmt.Errorf("%s.api_key_env is empty for an %s account", at, AccountAPIKey)
			}
		case AccountOAuth, AccountProxiedOAuth, AccountProxiedAPIKey:
			if a.APIKeyEnv != "" {
				return fmt.Errorf("%s.api_key_env is set for a %s account, which reads no key", at, a.Kind)
			}
		default:
			return fmt.Errorf("%s.kind: %q is not %q, %q, %q or %q", at, a.Kind,
				AccountOAuth, AccountAPIKey, AccountProxiedOAuth, AccountProxiedAPIKey)
		}
		if a.ChatGPTAccountID != "" && a.Kind != AccountProxiedOAuth {
			return fmt.Errorf("%s.chatgpt_account_id is set for a %s account: only a %s account sets it",
				at, a.Kind, AccountProxiedOAuth)
		}
	}
	return nil
}
