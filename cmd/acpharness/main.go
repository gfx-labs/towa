// Command acpharness is a coding agent that speaks the Agent Client Protocol.
// The command line lives in src/cli.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gfx-labs/acpharness/src/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := cli.Run(ctx, os.Args, cli.OSHost())
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "acpharness:", err)
		os.Exit(1)
	}
}
