package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
)

// loginEnv is what the account commands take from outside the configuration.
// The commands use the built-in flows and the desktop's browser. A test
// points the flows at an endpoint it runs and stands in for the browser.
type loginEnv struct {
	flows       map[string]auth.Flow
	openBrowser func(ctx context.Context, url string) error
}

// accountCommands returns the login, logout and accounts commands.
func accountCommands(h Host) []*cli.Command {
	return accountCommandsWith(h, loginEnv{flows: auth.Flows(), openBrowser: auth.OpenBrowser})
}

func accountCommandsWith(h Host, env loginEnv) []*cli.Command {
	var providers, device []string
	for provider, flow := range env.flows {
		providers = append(providers, provider)
		if flow.Device.UserCodeURL != "" {
			device = append(device, provider)
		}
	}
	slices.Sort(providers)
	slices.Sort(device)
	return []*cli.Command{
		{
			Name: "login",
			Usage: "log in to " + strings.Join(providers, " or ") +
				" and store the login in auth.json, creating the account when it is new",
			ArgsUsage: "<provider>",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "account", Usage: "the `id` the account has or gets: a model chain names it as provider:id/model"},
				&cli.BoolFlag{Name: "manual", Usage: "open no browser: print the page to open on any device, and take the code it ends on here or in a later run with --code"},
				&cli.BoolFlag{Name: "device", Usage: "print a one-time code and the page to enter it on, and wait for the approval (" + strings.Join(device, ", ") + ")"},
				&cli.StringFlag{Name: "code", Usage: "finish the login this provider is waiting on with the `code` or address its page ended on"},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				return login(ctx, cmd, h, env)
			},
		},
		{
			Name:  "logout",
			Usage: "delete an account's stored login",
			Flags: []cli.Flag{&cli.StringFlag{Name: "account", Usage: "the `id` of the account", Required: true}},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				return logout(ctx, cmd, h)
			},
		},
		{
			Name:  "accounts",
			Usage: "list the accounts of the configuration and of auth.json, and whether each can be used",
			Action: func(_ context.Context, cmd *cli.Command) error {
				return listAccounts(cmd, h)
			},
		},
	}
}

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
	}
	return config.Account{}, false, fmt.Errorf("account %q is of kind %s, which has no login: its key comes from %s", id, a.Kind, a.APIKeyEnv)
}

func login(ctx context.Context, cmd *cli.Command, h Host, env loginEnv) error {
	if cmd.Args().Len() != 1 {
		return fmt.Errorf("login takes one provider, and got %d arguments", cmd.Args().Len())
	}
	provider := cmd.Args().First()
	flow, ok := env.flows[provider]
	if !ok {
		return fmt.Errorf("provider %q has no OAuth login", provider)
	}
	manual, device, pasted := cmd.Bool("manual"), cmd.Bool("device"), cmd.String("code")
	if (manual && device) || (pasted != "" && (manual || device)) {
		return errors.New("--manual, --device and --code are three ways to log in: give one")
	}
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}

	id := cmd.String("account")
	var pending auth.PendingLogin
	if pasted != "" {
		// The login that printed the page named its account.
		pending, err = store.LoadPending(provider)
		if errors.Is(err, auth.ErrNoPendingLogin) {
			return fmt.Errorf("%w: start one with \"acpharness login %s --account <id> --manual\"", err, provider)
		}
		if err != nil {
			return err
		}
		if id != "" && id != pending.Account {
			return fmt.Errorf("the %s login that is waiting for a code is for account %q, not %q", provider, pending.Account, id)
		}
		id = pending.Account
	}
	if id == "" {
		return fmt.Errorf("login needs --account <id>: the id the account has or gets, which a model chain names as %s:<id>/<model>", provider)
	}
	// An id that cannot be stored is refused before the user is sent anywhere.
	_, err = store.Load(id)
	inStore := err == nil
	if err != nil && !errors.Is(err, auth.ErrNotLoggedIn) {
		return err
	}
	// No account of that id is not an error: the login makes it.
	account, found, err := loginAccount(&loaded.Config, id)
	if err != nil {
		return err
	}
	if found && account.Provider != provider {
		return fmt.Errorf("account %q belongs to provider %q, not %q", id, account.Provider, provider)
	}
	// An account that a config layer declares is not new, whatever the
	// store holds.
	exists := inStore || (found && !account.Stored)

	client := httpclient.New()
	tokenTimeout := time.Duration(loaded.Config.Auth.TokenTimeoutSeconds) * time.Second
	var tokens auth.Tokens
	if pasted != "" {
		code, state, err := auth.ParsePasted(pasted)
		if err != nil {
			return err
		}
		exchange, cancel := context.WithTimeout(ctx, tokenTimeout)
		defer cancel()
		if tokens, err = flow.Resume(pending).Exchange(exchange, client, code, state); err != nil {
			return err
		}
	} else {
		opts := auth.LoginOptions{
			Flow:            flow,
			HTTPClient:      client,
			Manual:          manual,
			Device:          device,
			OpenBrowser:     env.openBrowser,
			In:              h.Stdin,
			Out:             h.Stderr,
			CallbackTimeout: time.Duration(loaded.Config.Auth.LoginTimeoutSeconds) * time.Second,
			TokenTimeout:    tokenTimeout,
		}
		// A device login is approved by its own code and waits for no other.
		if !device {
			opts.Later = func(l *auth.Login) error { return store.SavePending(provider, l.Pending(id)) }
		}
		tokens, err = auth.RunLogin(ctx, opts)
		if errors.Is(err, auth.ErrNothingPasted) {
			// Nobody is at this terminal. The page is printed and the
			// login is recorded, so the later run is how it finishes.
			_, err = fmt.Fprintf(h.Stdout, "account %s: the %s login is waiting for its code: acpharness login %s --code '...'\n", id, provider, provider)
			return err
		}
		if err != nil {
			return err
		}
	}

	// Under the lock, so that a refresh of the old login that is running
	// in another process does not write over the new one.
	unlock, err := store.Lock(ctx, id)
	if err != nil {
		return err
	}
	if err := errors.Join(store.Save(id, tokens), unlock()); err != nil {
		return err
	}
	if !device {
		// The code is spent. Another login of the provider may have
		// replaced the record since, and then it is that login's.
		if err := store.DeletePending(provider); err != nil && !errors.Is(err, auth.ErrNoPendingLogin) {
			return err
		}
	}
	made := "is logged in"
	if !exists {
		made = "is created and logged in"
	}
	_, err = fmt.Fprintf(h.Stdout, "account %s %s to %s until %s, and is refreshed after that: a model chain names it as %s:%s/<model>\n",
		id, made, provider, tokens.ExpiresAt().Format(time.RFC3339), provider, id)
	return err
}

func logout(ctx context.Context, cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	id := cmd.String("account")
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

// listAccounts prints one line per account. It prints no token.
func listAccounts(cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
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
		case config.AccountAPIKey:
			state = a.APIKeyEnv + " is set"
			if lookup(h.Environ, a.APIKeyEnv) == "" {
				state = a.APIKeyEnv + " is not set"
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
		_, err = fmt.Fprintln(h.Stderr, "there are no accounts: create one with \"acpharness login <provider> --account <id>\"")
	}
	return err
}
