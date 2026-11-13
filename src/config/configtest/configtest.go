// Package configtest loads a configuration for tests.
package configtest

import (
	"github.com/alecthomas/kong"

	"github.com/gfx-labs/towa/src/config"
)

// Load resolves the configuration from src and args, the flags of a command
// line without commands.
func Load(src config.Sources, args ...string) (*config.Loaded, error) {
	var flags config.Flags
	_, loaded, err := config.Parse(src, &flags, &flags, args, kong.NoDefaultHelp())
	return loaded, err
}
