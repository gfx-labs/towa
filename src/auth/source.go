package auth

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/gfx-labs/towa/src/config"
)

// Errors of an AccountSource. Each is wrapped with the account's id.
var (
	// ErrUnknownAccount: no account has that id, in the configuration or
	// in the auth store.
	ErrUnknownAccount = errors.New("no such account")
	// ErrProxied: a proxy holds the account's credential, so the harness has
	// nothing to refresh, log in to or log out of.
	ErrProxied = errors.New("a proxy holds this account's credential")
	// ErrStaticKey: an API key from the environment, or from a command that
	// ran when the harness started, has no newer version.
	ErrStaticKey = errors.New("an API key cannot be refreshed")
	// ErrNoKey: the environment variable of an api_key account is not set,
	// or its command printed nothing.
	ErrNoKey = errors.New("the account's API key is not set")
	// ErrNoCredential: an account of kind none sends no credential, so a
	// refusal cannot be answered with a newer one.
	ErrNoCredential = errors.New("the account sends no credential")
)

// LoginRequiredError says the account's refresh token is dead: the token
// endpoint refused it for good. The account stays in the store and nothing
// is sent for it until a new login.
type LoginRequiredError struct {
	Account  string
	Provider string
	// RejectedAt is when the endpoint refused the refresh token, and Code
	// the code it gave.
	RejectedAt time.Time
	Code       string
	// Err is the endpoint's refusal, in the call that received it. Later
	// calls read the record and have none.
	Err error
}

func (e *LoginRequiredError) Error() string {
	return fmt.Sprintf("account %q: the token endpoint refused the stored %s login (%s) at %s: log in again with \"towa login %s --account %s\"",
		e.Account, e.Provider, e.Code, e.RejectedAt.Format(time.RFC3339), e.Provider, e.Account)
}

func (e *LoginRequiredError) Unwrap() error { return e.Err }

// deadTokenCodes are the token endpoint's codes that mean the refresh token
// will never work again. They are the OAuth codes among the markers jcode
// treats as permanent (crates/jcode-base/src/auth/refresh_state.rs).
// refresh_token_invalidated is OpenAI's own.
var deadTokenCodes = []string{"invalid_grant", "refresh_token_invalidated", "unauthorized_client"}

// deadTokenCode returns the code in err that says the refresh token is
// dead, or "" when err is anything else: a network failure, a 5xx or a
// refusal the endpoint may take back.
func deadTokenCode(err error) string {
	var refusal *TokenError
	if !errors.As(err, &refusal) || len(refusal.Detail) == 0 {
		return ""
	}
	// The "error" member is the code itself, or an object with the code.
	var code string
	if json.Unmarshal(refusal.Detail, &code) != nil {
		var detail struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(refusal.Detail, &detail) != nil {
			return ""
		}
		code = detail.Code
	}
	if slices.Contains(deadTokenCodes, code) {
		return code
	}
	return ""
}

func loginRequired(a config.Account, t Tokens, err error) *LoginRequiredError {
	return &LoginRequiredError{
		Account: a.ID, Provider: a.Provider,
		RejectedAt: time.UnixMilli(t.RefreshRejectedAtUnixMs), Code: t.RefreshRejection, Err: err,
	}
}

// PlaceholderToken is the token sent for a proxy-held account. It is not a
// secret. The proxy reads the account from it and puts the real one in its place.
func PlaceholderToken(account string) string { return "towa-proxied-" + account }

// Options is what an AccountSource is built from. Every field is required,
// except that RefreshMargin may be zero and Flows may be empty when no
// account is of kind oauth.
type Options struct {
	Accounts []config.Account
	Store    *Store
	// HTTPClient sends the refresh requests. It comes from src/support/httpclient.
	HTTPClient *http.Client
	// Getenv reads the environment variable of an api_key account.
	Getenv func(name string) string
	// Environ is the environment an api_key_command runs with, and
	// KeyCommandTimeout how long one may take. Both are needed only when an
	// account has such a command.
	Environ           []string
	KeyCommandTimeout time.Duration
	// Flows holds the OAuth flow of each provider that has oauth accounts.
	Flows map[string]Flow
	// RefreshMargin is how long before its expiry a token is refreshed, so
	// that a request does not leave with a token that expires on the way.
	RefreshMargin time.Duration
	// TokenTimeout bounds one refresh request. The others wait on the lock
	// for as long as the one that holds it takes.
	TokenTimeout time.Duration
}

// AccountSource is the Source over the configured accounts.
type AccountSource struct {
	opts     Options
	accounts map[string]config.Account
	// keys holds what each api_key_command printed, by account. It is
	// filled once, before the source is returned.
	keys map[string]string
}

var _ Source = (*AccountSource)(nil)

// NewSource checks that every account can be served, runs the
// api_key_command of each account that has one, and returns the source. A
// command that fails or prints nothing is an error.
func NewSource(ctx context.Context, opts Options) (*AccountSource, error) {
	switch {
	case opts.Store == nil:
		return nil, errors.New("auth: Options.Store is nil")
	case opts.HTTPClient == nil:
		return nil, errors.New("auth: Options.HTTPClient is nil")
	case opts.Getenv == nil:
		return nil, errors.New("auth: Options.Getenv is nil")
	case opts.RefreshMargin < 0:
		return nil, fmt.Errorf("auth: Options.RefreshMargin is %s", opts.RefreshMargin)
	case opts.TokenTimeout <= 0:
		return nil, fmt.Errorf("auth: Options.TokenTimeout is %s", opts.TokenTimeout)
	}
	s := &AccountSource{opts: opts, accounts: map[string]config.Account{}, keys: map[string]string{}}
	for _, a := range opts.Accounts {
		if _, ok := opts.Flows[a.Provider]; a.Kind == config.AccountOAuth && !ok {
			return nil, fmt.Errorf("account %q: provider %q has no OAuth login", a.ID, a.Provider)
		}
		s.accounts[a.ID] = a
		if len(a.APIKeyCommand) == 0 {
			continue
		}
		key, err := runKeyCommand(ctx, a.APIKeyCommand, opts.Environ, opts.KeyCommandTimeout)
		if err != nil {
			return nil, fmt.Errorf("account %q: api_key_command %s: %w", a.ID, a.APIKeyCommand[0], err)
		}
		s.keys[a.ID] = key
	}
	return s, nil
}

// runKeyCommand runs command and returns what it printed, without the space
// around it. Its arguments and its output are in no error: either may hold
// a secret. What it wrote to standard error is, since that is where a
// program says why it failed.
func runKeyCommand(ctx context.Context, command, environ []string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return "", fmt.Errorf("auth: Options.KeyCommandTimeout is %s", timeout)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command[0], command[1:]...) //nolint:gosec // the operator's configured command
	cmd.Env = environ
	// A child that keeps the pipes open is not waited for longer than the
	// command itself was.
	cmd.WaitDelay = timeout
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("did not finish in %s: %w", timeout, context.Cause(ctx))
		}
		if said := strings.TrimSpace(stderr.String()); said != "" {
			return "", fmt.Errorf("%w: %s", err, said)
		}
		return "", err
	}
	key := strings.TrimSpace(stdout.String())
	if key == "" {
		return "", ErrNoKey
	}
	return key, nil
}

// Credential implements Source.
func (s *AccountSource) Credential(ctx context.Context, account string) (Credential, error) {
	a, ok := s.accounts[account]
	if !ok {
		return Credential{}, fmt.Errorf("account %q: %w", account, ErrUnknownAccount)
	}
	switch a.Kind {
	case config.AccountAPIKey:
		if key, ok := s.keys[a.ID]; ok {
			return Credential{Account: a.ID, Scheme: SchemeAPIKey, Token: key}, nil
		}
		key := s.opts.Getenv(a.APIKeyEnv)
		if key == "" {
			return Credential{}, fmt.Errorf("account %q: %s: %w", a.ID, a.APIKeyEnv, ErrNoKey)
		}
		return Credential{Account: a.ID, Scheme: SchemeAPIKey, Token: key}, nil
	case config.AccountNone:
		return Credential{Account: a.ID, Scheme: SchemeNone}, nil
	case config.AccountProxiedAPIKey:
		return Credential{Account: a.ID, Scheme: SchemeAPIKey, Token: PlaceholderToken(a.ID), Proxied: true}, nil
	case config.AccountProxiedOAuth:
		return Credential{
			Account: a.ID, Scheme: SchemeOAuth, Token: PlaceholderToken(a.ID), Proxied: true,
			ChatGPTAccountID: a.ChatGPTAccountID,
		}, nil
	case config.AccountOAuth:
		// The common case takes no lock: Save replaces the file whole.
		t, err := s.load(a)
		if err != nil {
			return Credential{}, err
		}
		if t.RefreshRejectedAtUnixMs != 0 {
			return Credential{}, loginRequired(a, t, nil)
		}
		if s.fresh(t) {
			return credential(a, t), nil
		}
		return s.refresh(ctx, a, s.fresh)
	}
	return Credential{}, fmt.Errorf("account %q: kind %q is not one the auth source serves", a.ID, a.Kind)
}

// Refresh implements Source.
func (s *AccountSource) Refresh(ctx context.Context, rejected Credential) (Credential, error) {
	a, ok := s.accounts[rejected.Account]
	if !ok {
		return Credential{}, fmt.Errorf("account %q: %w", rejected.Account, ErrUnknownAccount)
	}
	switch a.Kind {
	case config.AccountAPIKey:
		return Credential{}, fmt.Errorf("account %q: %w", a.ID, ErrStaticKey)
	case config.AccountNone:
		return Credential{}, fmt.Errorf("account %q: %w", a.ID, ErrNoCredential)
	case config.AccountProxiedAPIKey, config.AccountProxiedOAuth:
		return Credential{}, fmt.Errorf("account %q: %w", a.ID, ErrProxied)
	case config.AccountOAuth:
		// Another process may have replaced the refused token already. The
		// refused one is not kept, though its expiry is still ahead.
		return s.refresh(ctx, a, func(t Tokens) bool {
			return t.AccessToken != rejected.Token && s.fresh(t)
		})
	}
	return Credential{}, fmt.Errorf("account %q: kind %q is not one the auth source serves", a.ID, a.Kind)
}

// refresh gets new tokens for a, unless the stored ones pass keep once the
// account's lock is held. Every process that found the token expired waits
// on the lock, and all but the first find the new token in the file.
func (s *AccountSource) refresh(ctx context.Context, a config.Account, keep func(Tokens) bool) (cred Credential, err error) {
	unlock, err := s.opts.Store.Lock(ctx, a.ID)
	if err != nil {
		return Credential{}, err
	}
	defer func() { err = errors.Join(err, unlock()) }()

	t, err := s.load(a)
	if err != nil {
		return Credential{}, err
	}
	// Another process may have found the refresh token dead while this one
	// waited for the lock.
	if t.RefreshRejectedAtUnixMs != 0 {
		return Credential{}, loginRequired(a, t, nil)
	}
	if keep(t) {
		return credential(a, t), nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.opts.TokenTimeout)
	defer cancel()
	old := t
	t, err = s.opts.Flows[a.Provider].Refresh(requestCtx, s.opts.HTTPClient, old)
	if code := deadTokenCode(err); code != "" {
		old.RefreshRejectedAtUnixMs, old.RefreshRejection = time.Now().UnixMilli(), code
		if saveErr := s.opts.Store.Save(a.ID, old); saveErr != nil {
			return Credential{}, fmt.Errorf("account %q: refresh: %w", a.ID, errors.Join(err, saveErr))
		}
		return Credential{}, loginRequired(a, old, err)
	}
	if err != nil {
		return Credential{}, fmt.Errorf("account %q: refresh: %w", a.ID, err)
	}
	// The endpoint has replaced the refresh token. The write must not be
	// lost to a cancelled context, and it does not take one.
	if err := s.opts.Store.Save(a.ID, t); err != nil {
		return Credential{}, fmt.Errorf("account %q: the refreshed token could not be stored: %w", a.ID, err)
	}
	return credential(a, t), nil
}

func (s *AccountSource) load(a config.Account) (Tokens, error) {
	t, err := s.opts.Store.Load(a.ID)
	if errors.Is(err, ErrNotLoggedIn) {
		return Tokens{}, fmt.Errorf("%w: log in with \"towa login %s --account %s\"", err, a.Provider, a.ID)
	}
	if err != nil {
		return Tokens{}, err
	}
	if t.Provider != a.Provider {
		return Tokens{}, fmt.Errorf("account %q: the stored login is for provider %q and the account is configured for %q",
			a.ID, t.Provider, a.Provider)
	}
	return t, nil
}

func (s *AccountSource) fresh(t Tokens) bool {
	return time.Now().Add(s.opts.RefreshMargin).Before(t.ExpiresAt())
}

func credential(a config.Account, t Tokens) Credential {
	return Credential{Account: a.ID, Scheme: SchemeOAuth, Token: t.AccessToken, ChatGPTAccountID: t.ChatGPTAccountID}
}
