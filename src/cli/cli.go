// Package cli is the towa command line: its commands, its flags and
// the wiring from the resolved configuration to a served connection.
// cmd/towa only calls Run.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/alecthomas/kong"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/cli/hosts"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/models"
	"github.com/gfx-labs/towa/src/provider/openai"
	"github.com/gfx-labs/towa/src/support/telemetry"
)

// Host is what the program takes from the machine it runs on.
type Host struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	CloseStdio     func() error
	// Environ is the environment, as os.Environ returns it.
	Environ []string
	// SystemDir is the directory of the system-wide config file. Empty
	// means there is none.
	SystemDir string
}

// OSHost is the Host of the running process.
func OSHost() Host {
	return Host{
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		CloseStdio: func() error {
			return errors.Join(os.Stdin.Close(), os.Stdout.Close())
		},
		Environ:   os.Environ(),
		SystemDir: "/etc/towa",
	}
}

// commandLine is the grammar kong parses: the flags that set the
// configuration, which every command takes, and the commands. A command is a
// struct with its own flags and a Run method, which kong calls with the
// values run binds.
type commandLine struct {
	config.Flags
	Version kong.VersionFlag `name:"version" short:"v" help:"Print the version."`

	Serve    serveCmd    `cmd:"" default:"1" help:"Serve ACP on standard input and output and on the sockets the listen keys name (the default command)."`
	Show     configCmd   `cmd:"" name:"config" help:"Print every config key with its value and the file, environment, flag or default it comes from."`
	Login    loginCmd    `cmd:"" help:"Log in to ${login_providers} and store the login in auth.json, creating the account when it is new."`
	Logout   logoutCmd   `cmd:"" help:"Delete an account's stored login."`
	Accounts accountsCmd `cmd:"" help:"List the accounts of the configuration and of auth.json, and whether each can be used."`
	Usage    usageCmd    `cmd:"" help:"Ask each subscription account's provider how much of its limits is used and when they reset."`
	MCP      mcpCmd      `cmd:"" name:"mcp" help:"Manage the OAuth logins of remote MCP servers."`
	Hosts    hostsCmd    `cmd:"" help:"List the hosts the resolved configuration contacts, one host:port and its config key per line."`
}

// commandEnv is what the commands take from outside the configuration. A
// test points it at endpoints it runs and stands in for the browser.
type commandEnv struct {
	login loginEnv
	mcp   mcpEnv
	hosts hostsEnv
}

// builtInEnv is the commandEnv of the program: the built-in flows and
// endpoints, and the desktop's browser.
func builtInEnv() commandEnv {
	return commandEnv{
		login: loginEnv{flows: auth.Flows(), openBrowser: auth.OpenBrowser},
		mcp:   mcpEnv{openBrowser: auth.OpenBrowser},
		hosts: hostsEnv{
			flows: auth.Flows(),
			builtIn: hosts.BuiltIn{
				Claude:      anthropic.DefaultBaseURL,
				CodexAPIKey: openai.DefaultBaseURL,
				CodexOAuth:  openai.DefaultCodexBaseURL,
			},
		},
	}
}

// Run runs the command line in args, where args[0] is the program name. It
// returns when the command has finished or ctx is cancelled.
func Run(ctx context.Context, args []string, h Host) error {
	return run(ctx, args, h, builtInEnv())
}

func run(ctx context.Context, args []string, h Host, env commandEnv) error {
	var providers, device []string
	for provider, flow := range env.login.flows {
		providers = append(providers, provider)
		if flow.Device.UserCodeURL != "" {
			device = append(device, provider)
		}
	}
	slices.Sort(providers)
	slices.Sort(device)

	var line commandLine
	// --help and --version print and ask kong to exit. Nothing runs after
	// either, whatever the rest of the command line or the files hold.
	printed := false
	parsed, loaded, err := config.Parse(sources(h), &line, &line.Flags, args[1:],
		kong.Name("towa"),
		kong.Description("A coding agent that speaks the Agent Client Protocol.\n\n"+
			"Every setting is a config key, and every key is a flag below: --limits.max_wall=30m sets\n"+
			"limits.max_wall. \"towa config\" lists each key with its value and where the value\n"+
			"comes from. A key is set by, later ones winning:\n\n"+
			"  1. "+filepath.Join(h.SystemDir, "config.yaml")+"\n"+
			"  2. $$XDG_CONFIG_HOME/towa/config.yaml, or ~/.config/towa/config.yaml\n"+
			"  3. the files in "+config.EnvFiles+" (separated like PATH), then each --config file\n"+
			"  4. the environment: TOWA_LIMITS__MAX_WALL=30m sets limits.max_wall\n"+
			"  5. the flags\n\n"+
			"A config file is YAML, and may also be config.yml or config.json. The keys that hold tables\n"+
			"(accounts, providers, hooks, mcp.servers, context.sections) are written in a file.\n"+
			"As a flag or in the environment, a table is one line of YAML."),
		kong.Writers(h.Stdout, h.Stderr),
		kong.Exit(func(int) { printed = true }),
		kong.ConfigureHelp(kong.HelpOptions{Compact: true, NoAppDescFormat: true}),
		kong.Vars{
			"version":          version(),
			"login_providers":  strings.Join(providers, " or "),
			"device_providers": strings.Join(device, ", "),
		},
	)
	if printed {
		return nil
	}
	if err != nil {
		return err
	}
	parsed.BindTo(ctx, (*context.Context)(nil))
	return parsed.Run(loaded, h, env.login, env.mcp, env.hosts)
}

// sources names the config layers this host has. The files of --config are
// the command line's, which config.Parse reads.
func sources(h Host) config.Sources {
	src := config.Sources{
		SystemDir: h.SystemDir,
		Environ:   h.Environ,
		Files:     filepath.SplitList(lookup(h.Environ, config.EnvFiles)),
	}
	// The accounts that logins made. Without XDG_STATE_HOME and HOME there
	// is no auth store, so there are none.
	if lookup(h.Environ, "XDG_STATE_HOME") != "" || lookup(h.Environ, "HOME") != "" {
		src.StoredAccounts = func() (map[string]string, error) {
			store, err := authStore(h)
			if err != nil {
				return nil, err
			}
			return store.Accounts()
		}
	}
	// The user's config directory, by the XDG rule. Without XDG_CONFIG_HOME
	// and HOME there is no such directory, so that layer is empty.
	if dir := lookup(h.Environ, "XDG_CONFIG_HOME"); dir != "" {
		src.UserDir = filepath.Join(dir, "towa")
	} else if home := lookup(h.Environ, "HOME"); home != "" {
		src.UserDir = filepath.Join(home, ".config", "towa")
	}
	return src
}

type serveCmd struct{}

// Run serves until the client closes standard input or ctx is cancelled.
func (serveCmd) Run(ctx context.Context, loaded *config.Loaded, h Host) error {
	tel, err := telemetry.Start(ctx, loaded.Config.Telemetry, version(), h.Stderr)
	if err != nil {
		return err
	}
	stopPprof := func() error { return nil }
	if address := loaded.Config.Telemetry.Pprof.Address; address != "" {
		if stopPprof, err = servePprof(ctx, address, tel.Logger); err != nil {
			return errors.Join(err, tel.Shutdown(context.WithoutCancel(ctx)))
		}
	}
	err = serveACP(ctx, loaded, h, tel)
	err = errors.Join(err, stopPprof())
	tel.Logger.Info("stopped", "err", err)
	// ctx may be cancelled already. The exporters' own timeouts bound this.
	return errors.Join(err, tel.Shutdown(context.WithoutCancel(ctx)))
}

// serveACP serves the configured transports until they end, then closes
// what it opened.
func serveACP(ctx context.Context, loaded *config.Loaded, h Host, tel *telemetry.Telemetry) error {
	factory, err := newRuntimeFactory(ctx, loaded.Config, h, tel)
	if err != nil {
		return err
	}
	registry := &session.Registry{}
	if selected, err := models.Select(loaded.Config.Models, factory.unlisted, ""); err == nil {
		for _, entry := range selected.Entries {
			if entry.Listed {
				tel.Logger.Debug("the model's figures are from the provider's model list",
					"model", entry.Model, "account", entry.Account,
					config.KeyModelContextWindow, entry.Metadata.ContextWindow, config.KeyModelMaxOutputTokens, entry.Metadata.MaxOutputTokens)
			}
			if entry.GuessedFrom != "" {
				tel.Logger.Info("model is not in the built-in table, a figure is guessed from its name",
					"model", entry.Model, "from", entry.GuessedFrom,
					config.KeyModelContextWindow, entry.Metadata.ContextWindow, config.KeyModelMaxOutputTokens, entry.Metadata.MaxOutputTokens)
			}
		}
	}
	if err := session.AddModels(registry, loaded.Config.Models, factory.unlisted); err != nil {
		return errors.Join(err, factory.Close())
	}
	host, err := session.NewHost(session.Options{
		Version:  version(),
		Open:     factory.Open,
		Logger:   tel.Logger,
		Tracer:   tel.Tracer,
		Meter:    tel.Meter,
		Session:  loaded.Config.Session,
		Registry: registry,
		Store:    factory.store,
		Supports: session.Supports{
			AdditionalDirectories: true,
			SystemPrompt:          true,
			ContextOverrides:      true,
			MCP:                   &acp.McpCapabilities{Http: true, Sse: true},
		},
		Mapper: loop.Mapper{},
	})
	if err != nil {
		return errors.Join(err, factory.Close())
	}
	err = serveListeners(ctx, loaded.Config.Listen, host, h, tel.Logger, h.CloseStdio)
	if ctx.Err() != nil && errors.Is(err, context.Cause(ctx)) {
		err = nil
	}
	err = errors.Join(err, host.Close(context.WithoutCancel(ctx)))
	return errors.Join(err, factory.Close())
}

type configCmd struct{}

// Run prints every config key with its value and the layer that set it. A
// value is written as a YAML file takes it.
func (configCmd) Run(loaded *config.Loaded, h Host) error {
	w := tabwriter.NewWriter(h.Stdout, 0, 0, 2, ' ', 0)
	for _, key := range slices.Sorted(maps.Keys(loaded.Values)) {
		if _, err := fmt.Fprintf(w, "%s = %s\t# %s\n", key, loaded.Values[key], loaded.Origin[key]); err != nil {
			return err
		}
	}
	return w.Flush()
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
