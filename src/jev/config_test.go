package jev_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/jev"
)

const account = "accounts:\n  - id: main\n    provider: openrouter\n    kind: api_key\n    api_key_env: OPENROUTER_API_KEY\n"

func loadConfig(t *testing.T, body string) (*config.Loaded, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return config.Load(config.Sources{Files: []string{path}})
}

// The design's own example, loaded from a file and turned into a client.
func TestTheJevSectionLoadsAndBuildsAClient(t *testing.T) {
	got, err := loadConfig(t, "jev:\n  route: openrouter\n  model: typesafe/jev-1.13\n  account: main\n"+account)
	require.NoError(t, err)
	_, err = jev.New(jev.Options{Config: got.Config.Jev, HTTPClient: httpclient.New()})
	require.NoError(t, err)

	// With no jev section there is no Jev, and that is valid.
	got, err = loadConfig(t, "")
	require.NoError(t, err)
	_, err = jev.New(jev.Options{Config: got.Config.Jev, HTTPClient: httpclient.New()})
	require.ErrorIs(t, err, jev.ErrDisabled)
}

func TestTheCustomRouteAcceptsHTTPOnlyOnTheOperatorsNetwork(t *testing.T) {
	section := func(base string) string {
		return "jev:\n  route: custom\n  model: m\n  base_url: \"" + base + "\"\n"
	}
	for _, base := range []string{
		"https://jev.example.org", "https://jev.example.org/v1",
		"http://localhost:8080", "http://127.0.0.1:8080", "http://[::1]:8080",
		"http://10.1.2.3:8080", "http://192.168.1.5", "http://172.16.0.9", "http://[fd00::1]:8080",
		"http://100.64.0.7:8080", "http://box.tailnet-name.ts.net", "http://jev.local:8080",
	} {
		_, err := loadConfig(t, section(base))
		require.NoError(t, err, base)
	}
	for _, base := range []string{
		"http://jev.example.org", "http://8.8.8.8:8080", "http://100.128.0.1", "http://172.32.0.1",
		"https://user:pass@jev.example.org", "https://jev.example.org/?key=1", "https://jev.example.org/#x",
		"ftp://jev.example.org", "jev.example.org", "https://",
	} {
		_, err := loadConfig(t, section(base))
		require.Error(t, err, base)
	}
}

func TestAJevSectionThatCannotWorkIsRejected(t *testing.T) {
	for name, body := range map[string]string{
		"unknown route":                  "jev:\n  route: jcode\n  account: main\n" + account,
		"model without a route":          "jev:\n  model: m\n",
		"account without a route":        "jev:\n  account: main\n" + account,
		"fixed route without an account": "jev:\n  route: typesafe\n",
		"account that is not configured": "jev:\n  route: typesafe\n  account: other\n" + account,
		"fixed route with a base url":    "jev:\n  route: aimlapi\n  account: main\n  base_url: https://x.example.org\n" + account,
		"custom without a base url":      "jev:\n  route: custom\n  model: m\n",
		"custom without a model":         "jev:\n  route: custom\n  base_url: https://x.example.org\n",
		"title off openrouter":           "jev:\n  route: typesafe\n  account: main\n  title: App\n" + account,
		"timeout over the maximum":       "jev:\n  timeout: 31s\n",
		"zero timeout":                   "jev:\n  timeout: 0s\n",
		"zero request limit":             "jev:\n  max_request_bytes: 0\n",
		"zero response limit":            "jev:\n  max_response_bytes: 0\n",
		"zero text limit":                "jev:\n  max_text_chars: 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(t, body)
			require.Error(t, err)
		})
	}
}
