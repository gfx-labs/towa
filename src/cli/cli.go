// Package cli is the acpharness command line: its commands, its flags and
// the wiring from the resolved configuration to a served connection.
// cmd/acpharness only calls Run.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/knadh/koanf/providers/cliflagv3"
	"github.com/knadh/koanf/v2"
	"github.com/urfave/cli/v3"

	"github.com/gfx-labs/acpharness/src/acp"
	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/session"
	"github.com/gfx-labs/acpharness/src/telemetry"
)

// Host is what the program takes from the machine it runs on.
type Host struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Environ is the environment, as os.Environ returns it.
	Environ []string
	// SystemDir is the directory of the system-wide config file. Empty
	// means there is none.
	SystemDir string
}

// OSHost is the Host of the running process.
func OSHost() Host {
	return Host{
		Stdin:     os.Stdin,
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Environ:   os.Environ(),
		SystemDir: "/etc/acpharness",
	}
}

// Run runs the command line in args, where args[0] is the program name. It
// returns when the command has finished or ctx is cancelled.
func Run(ctx context.Context, args []string, h Host) error {
	app := &cli.Command{
		Name:      "acpharness",
		Usage:     "a coding agent that speaks the Agent Client Protocol",
		Version:   version(),
		Reader:    h.Stdin,
		Writer:    h.Stdout,
		ErrWriter: h.Stderr,
		// --set values may hold commas.
		DisableSliceFlagSeparator: true,
		DefaultCommand:            "serve",
		Flags: []cli.Flag{
			&cli.StringSliceFlag{Name: "config", Usage: "read this config `file` after the system, user and " + config.EnvFiles + " files (repeatable)"},
			&cli.StringSliceFlag{Name: "set", Usage: "set a config `key=value` over every other source (repeatable)"},
			&cli.StringFlag{Name: "log-level", Usage: "stderr log `level`: debug, info, warn or error"},
			&cli.StringFlag{Name: "log-format", Usage: "stderr log `format`: " + config.FormatText + " or " + config.FormatJSON},
		},
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "serve ACP on standard input and output (the default command)",
				Action: func(ctx context.Context, cmd *cli.Command) error {
					return serve(ctx, cmd, h)
				},
			},
			{
				Name:  "config",
				Usage: "print the resolved configuration and where each value comes from",
				Action: func(_ context.Context, cmd *cli.Command) error {
					return printConfig(cmd, h)
				},
			},
		},
	}
	return app.Run(ctx, args)
}

// serve runs the agent for the one client on standard input and output. It
// returns when the client closes standard input or ctx is cancelled.
func serve(ctx context.Context, cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	tel, err := telemetry.Start(ctx, loaded.Config.Telemetry, version(), h.Stderr)
	if err != nil {
		return err
	}

	agent, err := session.NewAgent(session.Options{
		Version: version(),
		Turn:    loop.Stub,
		Logger:  tel.Logger,
		Tracer:  tel.Tracer,
		Meter:   tel.Meter,
	})
	if err != nil {
		return errors.Join(err, tel.Shutdown(context.WithoutCancel(ctx)))
	}
	conn := acp.NewAgentSideConnection(agent, h.Stdout, h.Stdin, tel.Logger)
	agent.SetClient(conn)

	tel.Logger.Info("serving ACP on stdio", "version", version())
	err = conn.Serve(ctx)
	if ctx.Err() != nil && errors.Is(err, context.Cause(ctx)) {
		// Stopped by a signal, which is a normal way to end.
		err = nil
	}
	tel.Logger.Info("stopped", "err", err)
	// ctx may be cancelled already. The exporters' own timeouts bound this.
	return errors.Join(err, tel.Shutdown(context.WithoutCancel(ctx)))
}

// printConfig prints every config key with its value and the layer that set it.
func printConfig(cmd *cli.Command, h Host) error {
	loaded, err := loadConfig(cmd, h)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(loaded.Values))
	for key := range loaded.Values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	w := tabwriter.NewWriter(h.Stdout, 0, 0, 2, ' ', 0)
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%s = %v\t# %s\n", key, loaded.Values[key], loaded.Origin[key]); err != nil {
			return err
		}
	}
	return w.Flush()
}

// loadConfig gathers the config layers of this invocation and loads them.
func loadConfig(cmd *cli.Command, h Host) (*config.Loaded, error) {
	src := config.Sources{
		SystemDir: h.SystemDir,
		Environ:   h.Environ,
		Flags:     map[string]any{},
		Set:       cmd.StringSlice("set"),
	}
	// The user's config directory, by the XDG rule. Without XDG_CONFIG_HOME
	// and HOME there is no such directory, so that layer is empty.
	if dir := lookup(h.Environ, "XDG_CONFIG_HOME"); dir != "" {
		src.UserDir = filepath.Join(dir, "acpharness")
	} else if home := lookup(h.Environ, "HOME"); home != "" {
		src.UserDir = filepath.Join(home, ".config", "acpharness")
	}
	src.Files = append(filepath.SplitList(lookup(h.Environ, config.EnvFiles)), cmd.StringSlice("config")...)

	// The provider yields the flags that were given, under the names of the
	// commands that declare them.
	given := koanf.New(".")
	if err := given.Load(cliflagv3.Provider(cmd, "."), nil); err != nil {
		return nil, err
	}
	for path, value := range given.All() {
		name := path[strings.LastIndex(path, ".")+1:]
		if key, ok := config.FlagKeys[name]; ok {
			src.Flags[key] = value
		}
	}
	return config.Load(src)
}

// lookup returns the value of name in environ, or "" when it is not set.
func lookup(environ []string, name string) string {
	value := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value = v
		}
	}
	return value
}

// version is the module version the binary was built from.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "unknown"
	}
	return info.Main.Version
}
