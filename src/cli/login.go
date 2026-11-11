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
	providers := make([]string, 0, len(env.flows))
	for provider := range env.flows {
		providers = append(providers, provider)
	}
	slices.Sort(providers)
	account := &cli.StringFlag{Name: "account", Usage: "the `id` of the account in the configuration", Required: true}
	return []*cli.Command{
		{
			Name:      "login",
			Usage:     "log an oauth account in and store its tokens (" + strings.Join(providers, ", ") + ")",
			ArgsUsage: "<provider>",
			Flags: []cli.Flag{
				account,
				&cli.BoolFlag{Name: "manual", Usage: "print the page to open and read a pasted code, for a host without a browser"},
			},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				return login(ctx, cmd, h, env)
			},
		},
		{
			Name:  "logout",
			Usage: "delete an oauth account's stored tokens",
			Flags: []cli.Flag{account},
			Action: func(ctx context.Context, cmd *cli.Command) error {
				return logout(ctx, cmd, h)
			},
		},
		{
			Name:  "accounts",
			Usage: "list the configured accounts and whether each can be used",
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

// storedAccount finds the account named by --account and checks that it is
// one whose tokens the harness holds.
func storedAccount(cmd *cli.Command, cfg *config.Config) (config.Account, error) {
	id := cmd.String("account")
	i := slices.IndexFunc(cfg.Accounts, func(a config.Account) bool { return a.ID == id })
	if i < 0 {
		return config.Account{}, fmt.Errorf("account %q: %w", id, auth.ErrUnknownAccount)
	}
	a := cfg.Accounts[i]
	switch a.Kind {
	case config.AccountOAuth:
		return a, nil
	case config.AccountProxiedOAuth, config.AccountProxiedAPIKey:
		return config.Account{}, fmt.Errorf("account %q: %w, so the harness stores no login for it", id, auth.ErrProxied)
	}
	return config.Account{}, fmt.Errorf("account %q is of kind %s, which has no login: its key comes from %s", id, a.Kind, a.APIKeyEnv)
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
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	account, err := storedAccount(cmd, &loaded.Config)
	if err != nil {
		return err
	}
	if account.Provider != provider {
		return fmt.Errorf("account %q is configured for provider %q, not %q", account.ID, account.Provider, provider)
	}
	store, err := authStore(h)
	if err != nil {
		return err
	}

	tokens, err := auth.RunLogin(ctx, auth.LoginOptions{
		Flow:            flow,
		HTTPClient:      httpclient.New(),
		Manual:          cmd.Bool("manual"),
		OpenBrowser:     env.openBrowser,
		In:              h.Stdin,
		Out:             h.Stderr,
		CallbackTimeout: time.Duration(loaded.Config.Auth.LoginTimeoutSeconds) * time.Second,
		TokenTimeout:    time.Duration(loaded.Config.Auth.TokenTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	// Under the lock, so that a refresh of the old login that is running
	// in another process does not write over the new one.
	unlock, err := store.Lock(ctx, account.ID)
	if err != nil {
		return err
	}
	if err := errors.Join(store.Save(account.ID, tokens), unlock()); err != nil {
		return err
	}
	_, err = fmt.Fprintf(h.Stdout, "account %s is logged in to %s until %s, and is refreshed after that\n",
		account.ID, provider, tokens.ExpiresAt().Format(time.RFC3339))
	return err
}

func logout(ctx context.Context, cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	account, err := storedAccount(cmd, &loaded.Config)
	if err != nil {
		return err
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
	_, err = fmt.Fprintf(h.Stdout, "account %s: the stored login is deleted (it is not revoked at the provider)\n", account.ID)
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
	return w.Flush()
}
