package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"text/tabwriter"
	"time"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/support/httpclient"
)

// loginEnv is what the account commands take from outside the configuration.
// The commands use the built-in flows and the desktop's browser. A test
// points the flows at an endpoint it runs and stands in for the browser.
type loginEnv struct {
	flows       map[string]auth.Flow
	openBrowser func(ctx context.Context, url string) error
}

// --manual, --device and --code are three ways to log in, so kong takes one.
type loginCmd struct {
	Provider string `arg:"" help:"The provider to log in to: ${login_providers}."`
	Account  string `placeholder:"ID" help:"The id the account has or gets: a model chain names it as provider:id/model."`
	Manual   bool   `xor:"way" help:"Open no browser: print the page to open on any device, and take the code it ends on here or in a later run with --code."`
	Device   bool   `xor:"way" help:"Print a one-time code and the page to enter it on, and wait for the approval (${device_providers})."`
	Code     string `xor:"way" placeholder:"CODE" help:"Finish the login this provider is waiting on with the code or address its page ended on."`
}

type logoutCmd struct {
	Account string `required:"" placeholder:"ID" help:"The id of the account."`
}

type accountsCmd struct{}

// authStore returns the store in the state directory of h's environment.
func authStore(h Host) (*auth.Store, error) {
	dir, err := auth.StateDir(lookup(h.Environ, "XDG_STATE_HOME"), lookup(h.Environ, "HOME"))
	if err != nil {
		return nil, err
	}
	return auth.NewStore(dir), nil
}

// loginAccount finds the account a login or logout is for: one whose login
// the harness holds. ok is false when no account has the id.
func loginAccount(cfg *config.Config, id string) (account config.Account, ok bool, err error) {
	i := slices.IndexFunc(cfg.Accounts, func(a config.Account) bool { return a.ID == id })
	if i < 0 {
		return config.Account{}, false, nil
	}
	a := cfg.Accounts[i]
	switch a.Kind {
	case config.AccountOAuth:
		return a, true, nil
	case config.AccountProxiedOAuth, config.AccountProxiedAPIKey:
		return config.Account{}, false, fmt.Errorf("account %q: %w, so the harness stores no login for it", id, auth.ErrProxied)
	case config.AccountNone:
		return config.Account{}, false, fmt.Errorf("account %q is of kind %s, which has no login: its endpoint takes no credential", id, a.Kind)
	}
	from := a.APIKeyEnv
	if len(a.APIKeyCommand) > 0 {
		from = "the command " + a.APIKeyCommand[0]
	}
	return config.Account{}, false, fmt.Errorf("account %q is of kind %s, which has no login: its key comes from %s", id, a.Kind, from)
}

// Run logs in to the provider and stores the login.
func (c *loginCmd) Run(ctx context.Context, loaded *config.Loaded, h Host, env loginEnv) error {
	flow, ok := env.flows[c.Provider]
	if !ok {
		return fmt.Errorf("provider %q has no OAuth login", c.Provider)
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}
	l := &loginRun{cmd: c, cfg: loaded.Config, host: h, env: env, flow: flow, store: store, client: httpclient.New()}
	if err := l.resolveAccount(); err != nil {
		return err
	}
	tokens, waiting, err := l.obtainTokens(ctx)
	if err != nil || waiting {
		return err
	}
	return l.finish(ctx, tokens)
}

// loginRun is one login: what the command and its environment give, and
// what the steps find along the way.
type loginRun struct {
	cmd    *loginCmd
	cfg    config.Config
	host   Host
	env    loginEnv
	flow   auth.Flow
	store  *auth.Store
	client *http.Client

	id      string
	pending auth.PendingLogin
	// exists is true when the account is not new, so the login does not
	// create it.
	exists bool
}

// resolveAccount settles the account id and whether the account exists,
// and refuses a login that cannot go ahead before the user is sent anywhere.
func (l *loginRun) resolveAccount() error {
	provider := l.cmd.Provider
	l.id = l.cmd.Account
	if l.cmd.Code != "" {
		// The login that printed the page named its account.
		pending, err := l.store.LoadPending(provider)
		if errors.Is(err, auth.ErrNoPendingLogin) {
			return fmt.Errorf("%w: start one with \"towa login %s --account <id> --manual\"", err, provider)
		}
		if err != nil {
			return err
		}
		if l.id != "" && l.id != pending.Account {
			return fmt.Errorf("the %s login that is waiting for a code is for account %q, not %q", provider, pending.Account, l.id)
		}
		l.pending, l.id = pending, pending.Account
	}
	if l.id == "" {
		return fmt.Errorf("login needs --account <id>: the id the account has or gets, which a model chain names as %s:<id>/<model>", provider)
	}
	// An id that cannot be stored is refused before the user is sent anywhere.
	_, err := l.store.Load(l.id)
	inStore := err == nil
	if err != nil && !errors.Is(err, auth.ErrNotLoggedIn) {
		return err
	}
	// No account of that id is not an error: the login makes it.
	account, found, err := loginAccount(&l.cfg, l.id)
	if err != nil {
		return err
	}
	if found && account.Provider != provider {
		return fmt.Errorf("account %q belongs to provider %q, not %q", l.id, account.Provider, provider)
	}
	// An account that a config layer declares is not new, whatever the
	// store holds.
	l.exists = inStore || (found && !account.Stored)
	return nil
}

// obtainTokens exchanges a pasted code or runs the login. waiting is true
// when nobody is at the terminal: the page is printed and the login is
// recorded, so a later run finishes it.
func (l *loginRun) obtainTokens(ctx context.Context) (tokens auth.Tokens, waiting bool, err error) {
	tokenTimeout := time.Duration(l.cfg.Auth.TokenTimeoutSeconds) * time.Second
	if pasted := l.cmd.Code; pasted != "" {
		code, state, err := auth.ParsePasted(pasted)
		if err != nil {
			return auth.Tokens{}, false, err
		}
		exchange, cancel := context.WithTimeout(ctx, tokenTimeout)
		defer cancel()
		tokens, err = l.flow.Resume(l.pending).Exchange(exchange, l.client, code, state)
		return tokens, false, err
	}
	device := l.cmd.Device
	opts := auth.LoginOptions{
		Flow:            l.flow,
		HTTPClient:      l.client,
		Manual:          l.cmd.Manual,
		Device:          device,
		OpenBrowser:     l.env.openBrowser,
		In:              l.host.Stdin,
		Out:             l.host.Stderr,
		CallbackTimeout: time.Duration(l.cfg.Auth.LoginTimeoutSeconds) * time.Second,
		TokenTimeout:    tokenTimeout,
	}
	// A device login is approved by its own code and waits for no other.
	if !device {
		opts.Later = func(login *auth.Login) error {
			l.pending = login.Pending(l.id)
			return l.store.SavePending(ctx, l.cmd.Provider, l.pending)
		}
	}
	tokens, err = auth.RunLogin(ctx, opts)
	if errors.Is(err, auth.ErrNothingPasted) {
		_, err = fmt.Fprintf(l.host.Stdout, "account %s: the %s login is waiting for its code: towa login %s --code '...'\n", l.id, l.cmd.Provider, l.cmd.Provider)
		return auth.Tokens{}, true, err
	}
	return tokens, false, err
}

// finish stores the tokens, clears the pending record and reports.
func (l *loginRun) finish(ctx context.Context, tokens auth.Tokens) error {
	provider := l.cmd.Provider
	// Under the lock, so that a refresh of the old login that is running
	// in another process does not write over the new one.
	unlock, err := l.store.Lock(ctx, l.id)
	if err != nil {
		return err
	}
	if err := errors.Join(l.store.Save(l.id, tokens), unlock()); err != nil {
		return err
	}
	if !l.cmd.Device {
		// The code is spent. Another login of the provider may have
		// replaced the record since, and then it is that login's.
		if err := l.store.DeletePending(ctx, provider, l.pending); err != nil && !errors.Is(err, auth.ErrNoPendingLogin) {
			return err
		}
	}
	made := "is logged in"
	if !l.exists {
		made = "is created and logged in"
	}
	_, err = fmt.Fprintf(l.host.Stdout, "account %s %s to %s until %s, and is refreshed after that: a model chain names it as %s:%s/<model>\n",
		l.id, made, provider, tokens.ExpiresAt().Format(time.RFC3339), provider, l.id)
	return err
}

// Run deletes the account's stored login.
func (c *logoutCmd) Run(ctx context.Context, loaded *config.Loaded, h Host) error {
	id := c.Account
	account, exists, err := loginAccount(&loaded.Config, id)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("account %q: %w", id, auth.ErrUnknownAccount)
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}
	unlock, err := store.Lock(ctx, account.ID)
	if err != nil {
		return err
	}
	if err := errors.Join(store.Delete(account.ID), unlock()); err != nil {
		return err
	}
	// The tokens are not revoked at the provider: neither upstream CLI
	// does, and jcode names no revocation endpoint.
	what := "the stored login is deleted"
	if account.Stored {
		what = "the account and its login are removed from auth.json"
	}
	_, err = fmt.Fprintf(h.Stdout, "account %s: %s (the login is not revoked at the provider)\n", account.ID, what)
	return err
}

// Run prints one line per account. It prints no token.
func (accountsCmd) Run(loaded *config.Loaded, h Host) error {
	store, err := authStore(h)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(h.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ACCOUNT\tPROVIDER\tKIND\tSTATE"); err != nil {
		return err
	}
	for _, a := range loaded.Config.Accounts {
		var state string
		switch a.Kind {
		case config.AccountProxiedOAuth, config.AccountProxiedAPIKey:
			state = "held by a proxy"
		case config.AccountNone:
			state = "no credential"
		case config.AccountAPIKey:
			switch {
			case len(a.APIKeyCommand) > 0:
				// Listing the accounts does not run the command.
				state = "key from the command " + a.APIKeyCommand[0]
			case lookup(h.Environ, a.APIKeyEnv) == "":
				state = a.APIKeyEnv + " is not set"
			default:
				state = a.APIKeyEnv + " is set"
			}
		case config.AccountOAuth:
			tokens, err := store.Load(a.ID)
			switch {
			case errors.Is(err, auth.ErrNotLoggedIn):
				state = "not logged in"
			case err != nil:
				return err
			case tokens.Provider != a.Provider:
				state = "the stored login is for " + tokens.Provider
			default:
				state = "logged in, token expires " + tokens.ExpiresAt().Format(time.RFC3339)
			}
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", a.ID, a.Provider, a.Kind, state); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if len(loaded.Config.Accounts) == 0 {
		_, err = fmt.Fprintln(h.Stderr, "there are no accounts: create one with \"towa login <provider> --account <id>\"")
	}
	return err
}

// terminalLogins are the logins a client can run for the user: one per
// account whose token the harness holds, run as "towa login" on the
// command line the client starts towa with.
func terminalLogins(accounts []config.Account, flows map[string]auth.Flow) []acp.AuthMethodTerminalInline {
	var methods []acp.AuthMethodTerminalInline
	for _, a := range accounts {
		if _, ok := flows[a.Provider]; !ok || a.Kind != config.AccountOAuth {
			continue
		}
		description := fmt.Sprintf("Log in to %s in the browser and store the login in auth.json.", a.Provider)
		methods = append(methods, acp.AuthMethodTerminalInline{
			Id:          acp.AuthMethodId("login-" + a.ID),
			Name:        fmt.Sprintf("Log in account %s (%s)", a.ID, a.Provider),
			Description: &description,
			Args:        []string{"login", a.Provider, "--account", a.ID},
		})
	}
	return methods
}
