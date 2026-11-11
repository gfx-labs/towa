package cli

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/hosts"
)

// hostsCommand returns the hosts command. Standard output holds one
// "host:port<TAB>key" line per host and nothing else, so that it can be
// piped into an allowlist. What the list does not cover goes to standard
// error.
func hostsCommand(h Host) *cli.Command {
	return hostsCommandWith(h, auth.Flows())
}

func hostsCommandWith(h Host, flows map[string]auth.Flow) *cli.Command {
	return &cli.Command{
		Name:  "hosts",
		Usage: "list the hosts the resolved configuration contacts, one host:port and its config key per line",
		Action: func(_ context.Context, cmd *cli.Command) error {
			loaded, err := loadConfig(cmd, h)
			if err != nil {
				return err
			}
			report, err := hosts.List(loaded.Config, hosts.Inputs{
				Flows: flows,
				OTLP: hosts.OTLP{
					Endpoint:        lookup(h.Environ, hosts.EnvOTLPEndpoint),
					TracesEndpoint:  lookup(h.Environ, hosts.EnvOTLPTracesEndpoint),
					MetricsEndpoint: lookup(h.Environ, hosts.EnvOTLPMetricsEndpoint),
					LogsEndpoint:    lookup(h.Environ, hosts.EnvOTLPLogsEndpoint),
				},
			})
			if err != nil {
				return err
			}
			for _, host := range report.Hosts {
				if _, err := fmt.Fprintf(h.Stdout, "%s\t%s\n", host.Address, host.Key); err != nil {
					return err
				}
			}
			for _, gap := range report.NotCovered {
				if _, err := fmt.Fprintf(h.Stderr, "hosts: not covered: %s\n", gap); err != nil {
					return err
				}
			}
			return nil
		},
	}
}
