package hooks_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
)

func loadConfig(t *testing.T, body string) (*config.Loaded, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return config.Load(config.Sources{Files: []string{path}})
}

// Hooks are a list of tables holding a list and a duration, which a real
// file must decode into the typed struct.
func TestHooksDecodeFromAFile(t *testing.T) {
	got, err := loadConfig(t, `
[[hooks]]
event = "pre_tool"
tools = ["Bash", "shell"]
command = ["/usr/local/bin/check-cmd", "--strict"]
timeout = "10s"
max_output_bytes = 65536

[[hooks]]
event = "stop"
command = ["/usr/local/bin/on-stop"]
timeout = "1m"
`)
	require.NoError(t, err)
	require.Equal(t, []config.Hook{
		{
			Event: config.HookPreTool, Tools: []string{"Bash", "shell"},
			Command: []string{"/usr/local/bin/check-cmd", "--strict"}, Timeout: 10 * time.Second, MaxOutputBytes: 65536,
		},
		{Event: config.HookStop, Command: []string{"/usr/local/bin/on-stop"}, Timeout: time.Minute},
	}, got.Config.Hooks)
}

func TestHooksThatCannotBeRunAreRejected(t *testing.T) {
	for name, body := range map[string]string{
		"unknown event":       "event = \"before\"\ncommand = [\"x\"]\ntimeout = \"1s\"\n",
		"no event":            "command = [\"x\"]\ntimeout = \"1s\"\n",
		"no command":          "event = \"stop\"\ntimeout = \"1s\"\n",
		"empty program":       "event = \"stop\"\ncommand = [\"\"]\ntimeout = \"1s\"\n",
		"no timeout":          "event = \"stop\"\ncommand = [\"x\"]\n",
		"negative timeout":    "event = \"stop\"\ncommand = [\"x\"]\ntimeout = \"-1s\"\n",
		"tools on a stop":     "event = \"stop\"\ntools = [\"Bash\"]\ncommand = [\"x\"]\ntimeout = \"1s\"\n",
		"tools on a prompt":   "event = \"prompt\"\ntools = [\"Bash\"]\ncommand = [\"x\"]\ntimeout = \"1s\"\n",
		"empty tool name":     "event = \"pre_tool\"\ntools = [\"\"]\ncommand = [\"x\"]\ntimeout = \"1s\"\n",
		"negative output cap": "event = \"stop\"\ncommand = [\"x\"]\ntimeout = \"1s\"\nmax_output_bytes = -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(t, "[[hooks]]\n"+body)
			require.Error(t, err)
		})
	}
}
