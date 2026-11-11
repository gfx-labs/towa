package jev_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/jev"
)

const account = "[[accounts]]\nid = \"main\"\nprovider = \"openrouter\"\nkind = \"api_key\"\napi_key_env = \"OPENROUTER_API_KEY\"\n"

func loadConfig(t *testing.T, body string) (*config.Loaded, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "c.toml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return config.Load(config.Sources{Files: []string{path}})
}

// The design's own example, loaded from a file and turned into a client.
func TestTheJevSectionLoadsAndBuildsAClient(t *testing.T) {
	got, err := loadConfig(t, "[jev]\nroute = \"openrouter\"\nmodel = \"typesafe/jev-1.13\"\naccount = \"main\"\n\n"+account)
	require.NoError(t, err)
	cfg := got.Config.Jev
	require.Equal(t, config.Jev{
		Route: config.JevOpenRouter, Model: "typesafe/jev-1.13", Account: "main",
		Timeout: 10 * time.Second, MaxTimeout: 30 * time.Second,
		MaxRequestBytes: 80 << 10, MaxResponseBytes: 256 << 10, MaxTextChars: 4000,
	}, cfg)
	client, err := jev.New(jev.Options{Config: cfg, HTTPClient: httpclient.New()})
	require.NoError(t, err)
	require.Equal(t, "typesafe/jev-1.13", client.Model())

	// With no [jev] section there is no Jev, and that is valid.
	got, err = loadConfig(t, "")
	require.NoError(t, err)
	_, err = jev.New(jev.Options{Config: got.Config.Jev, HTTPClient: httpclient.New()})
	require.ErrorIs(t, err, jev.ErrDisabled)
}

func TestTheCustomRouteAcceptsHTTPOnlyOnTheOperatorsNetwork(t *testing.T) {
	section := func(base string) string {
		return "[jev]\nroute = \"custom\"\nmodel = \"m\"\nbase_url = \"" + base + "\"\n"
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
		"unknown route":                  "[jev]\nroute = \"jcode\"\naccount = \"main\"\n" + account,
		"model without a route":          "[jev]\nmodel = \"m\"\n",
		"account without a route":        "[jev]\naccount = \"main\"\n" + account,
		"fixed route without an account": "[jev]\nroute = \"typesafe\"\n",
		"account that is not configured": "[jev]\nroute = \"typesafe\"\naccount = \"other\"\n" + account,
		"fixed route with a base url":    "[jev]\nroute = \"aimlapi\"\naccount = \"main\"\nbase_url = \"https://x.example.org\"\n" + account,
		"custom without a base url":      "[jev]\nroute = \"custom\"\nmodel = \"m\"\n",
		"custom without a model":         "[jev]\nroute = \"custom\"\nbase_url = \"https://x.example.org\"\n",
		"title off openrouter":           "[jev]\nroute = \"typesafe\"\naccount = \"main\"\ntitle = \"App\"\n" + account,
		"timeout over the maximum":       "[jev]\ntimeout = \"31s\"\n",
		"zero timeout":                   "[jev]\ntimeout = \"0s\"\n",
		"zero request limit":             "[jev]\nmax_request_bytes = 0\n",
		"zero response limit":            "[jev]\nmax_response_bytes = 0\n",
		"zero text limit":                "[jev]\nmax_text_chars = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadConfig(t, body)
			require.Error(t, err)
		})
	}
}
