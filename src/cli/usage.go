package cli

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/provider"
)

type usageCmd struct {
	Account string `placeholder:"ID" help:"Report this account alone. Without it every subscription account is reported."`
	JSON    bool   `name:"json" help:"Print one JSON array in place of the table."`
}

// accountUsage is one account's row of the usage command, and its JSON.
type accountUsage struct {
	Account  string       `json:"account"`
	Provider string       `json:"provider"`
	Plan     string       `json:"plan,omitzero"`
	Limits   []usageLimit `json:"limits"`
	// Error is why the usage was not read.
	Error string `json:"error,omitzero"`
}

type usageLimit struct {
	Name        string    `json:"name"`
	UsedPercent float64   `json:"used_percent"`
	ResetsAt    time.Time `json:"resets_at,omitzero"`
}

// Run asks each subscription account's provider how much of its limits the
// account has used, and prints it. An account whose usage cannot be read is
// printed with the reason, and the command then fails.
func (c *usageCmd) Run(ctx context.Context, loaded *config.Loaded, h Host) error {
	client := httpclient.New()
	defer client.CloseIdleConnections()
	providers, err := NewProviders(loaded.Config, h, client)
	if err != nil {
		return err
	}
	defer providers.Close() //nolint:errcheck // no request was streamed, so there is no socket to report on

	var accounts []config.Account
	for _, a := range loaded.Config.Accounts {
		_, reports := providers.clients[a.Provider].(provider.UsageReporter)
		subscription := a.Kind == config.AccountOAuth || a.Kind == config.AccountProxiedOAuth
		switch {
		case c.Account == a.ID:
			if !reports {
				return fmt.Errorf("account %q: provider %q reports no subscription usage", a.ID, a.Provider)
			}
			accounts = append(accounts, a)
		case c.Account == "" && reports && subscription:
			accounts = append(accounts, a)
		}
	}
	if c.Account != "" && len(accounts) == 0 {
		return fmt.Errorf("account %q is not configured: \"towa accounts\" lists the accounts", c.Account)
	}
	slices.SortFunc(accounts, func(a, b config.Account) int { return strings.Compare(a.ID, b.ID) })

	var failed []error
	rows := make([]accountUsage, 0, len(accounts))
	for _, a := range accounts {
		row := accountUsage{Account: a.ID, Provider: a.Provider, Limits: []usageLimit{}}
		usage, err := accountUsageOf(ctx, providers, a)
		if err != nil {
			row.Error = err.Error()
			failed = append(failed, fmt.Errorf("account %q: %w", a.ID, err))
		}
		row.Plan = usage.Plan
		for _, limit := range usage.Limits {
			row.Limits = append(row.Limits, usageLimit{Name: limit.Name, UsedPercent: limit.UsedPercent, ResetsAt: limit.ResetsAt})
		}
		rows = append(rows, row)
	}
	if c.JSON {
		if err := json.MarshalWrite(h.Stdout, rows, jsontext.Multiline(true)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(h.Stdout); err != nil {
			return err
		}
		return errors.Join(failed...)
	}
	if err := printUsage(h, rows); err != nil {
		return err
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprintln(h.Stderr, "there is no subscription account: create one with \"towa login <provider> --account <id>\""); err != nil {
			return err
		}
	}
	return errors.Join(failed...)
}

func accountUsageOf(ctx context.Context, providers *Providers, a config.Account) (provider.Usage, error) {
	cred, err := providers.Credentials.Credential(ctx, a.ID)
	if err != nil {
		return provider.Usage{}, err
	}
	return providers.clients[a.Provider].(provider.UsageReporter).Usage(ctx, cred)
}

func printUsage(h Host, rows []accountUsage) error {
	w := tabwriter.NewWriter(h.Stdout, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ACCOUNT\tPROVIDER\tPLAN\tLIMIT\tUSED\tRESETS"); err != nil {
		return err
	}
	dash := func(s string) string {
		if s == "" {
			return "-"
		}
		return s
	}
	for _, row := range rows {
		limits := row.Limits
		if len(limits) == 0 {
			// An account with no limit in use, or one that was not read,
			// still gets its line.
			state := "no limit is in use"
			if row.Error != "" {
				state = row.Error
			}
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t\t\n", row.Account, row.Provider, dash(row.Plan), state); err != nil {
				return err
			}
		}
		for _, limit := range limits {
			resets := ""
			if !limit.ResetsAt.IsZero() {
				resets = limit.ResetsAt.Local().Format(time.RFC3339)
			}
			used := strconv.FormatFloat(limit.UsedPercent, 'f', -1, 64) + "%"
			if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", row.Account, row.Provider, dash(row.Plan), limit.Name, used, dash(resets)); err != nil {
				return err
			}
		}
	}
	return w.Flush()
}
