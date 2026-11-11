package config

import "fmt"

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
	}
	return nil
}
