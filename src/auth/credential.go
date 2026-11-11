// Package auth holds the accounts acpharness can use and hands out their
// credentials.
package auth

import "context"

// Scheme is how a credential is presented to a provider. It decides the
// shape of the whole request, not only the authorization header: a Claude
// OAuth request carries a billing block and beta headers that an API key
// request does not.
type Scheme string

// Schemes of a credential.
const (
	SchemeOAuth  Scheme = "oauth"
	SchemeAPIKey Scheme = "api_key"
)

// Credential is what a provider client needs to authorize one request.
type Credential struct {
	// Account is the id of the account in the configuration.
	Account string
	Scheme  Scheme
	// Token is the secret to send. When Proxied is set it is a placeholder
	// that names the account and is not a secret.
	Token string
	// Proxied says the harness does not hold this account's token. A proxy
	// between the harness and the provider replaces the authorization. The
	// provider client builds the request exactly as it would for a token it
	// held, because the proxy changes nothing else.
	Proxied bool
	// ChatGPTAccountID is the account id the Codex backend wants in a
	// header. It is empty for every other provider.
	ChatGPTAccountID string
}

// Source hands out credentials by account id.
type Source interface {
	// Credential returns a credential that is valid now, refreshing a
	// harness-held OAuth token that has expired.
	Credential(ctx context.Context, account string) (Credential, error)
	// Refresh is called after a provider refused the credential it is
	// given. It returns a new credential when there is one to get. For an
	// API key or a proxied account there is none, and the error says so.
	Refresh(ctx context.Context, rejected Credential) (Credential, error)
}
