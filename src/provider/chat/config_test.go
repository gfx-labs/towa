package chat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/provider"
)

func loadProviders(t *testing.T, toml string) (*config.Loaded, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(toml), 0o600))
	return config.Load(config.Sources{Files: []string{path}})
}

// A provider written in a config file reaches the request body: the file's
// keys decode into the fields the client reads.
func TestAConfiguredOpenRouterProviderShapesTheRequest(t *testing.T) {
	loaded, err := loadProviders(t, `
[[providers]]
id = "openrouter"
kind = "chat"
base_url = "https://openrouter.ai/api/v1"
referer = "https://github.com/gfx-labs/acpharness"
title = "acpharness"
cache_key_field = "session_id"

[providers.openrouter]
enabled = true

[providers.openrouter.routing]
only = ["anthropic"]
allow_fallbacks = false
zdr = true
sort = "price"

[providers.openrouter.routing.max_price]
prompt = 3.0
`)
	require.NoError(t, err)
	require.Len(t, loaded.Config.Providers, 1)

	body, perr := newClient(t, loaded.Config.Providers[0]).body(provider.Request{Model: "m", Items: userSays("hi"), CacheKey: "s"})
	require.Nil(t, perr)
	require.JSONEq(t, `{
		"model": "m", "stream": true, "session_id": "s",
		"provider": {"only": ["anthropic"], "allow_fallbacks": false, "zdr": true, "sort": "price", "max_price": {"prompt": 3}},
		"messages": [{"role": "user", "content": "hi"}]
	}`, string(body))
}

func TestProviderConfigThatWouldNotBeSentIsAnError(t *testing.T) {
	base := "[[providers]]\nid = \"p\"\nbase_url = \"https://example.com/v1\"\n"
	cases := map[string]string{
		"routing without enabled": base + "kind = \"chat\"\n[providers.openrouter.routing]\nsort = \"price\"\n",
		"openrouter on responses": base + "kind = \"responses\"\n[providers.openrouter]\nenabled = true\n",
		"unknown sort":            base + "kind = \"chat\"\n[providers.openrouter]\nenabled = true\n[providers.openrouter.routing]\nsort = \"cheapest\"\n",
		"unknown max price":       base + "kind = \"chat\"\n[providers.openrouter]\nenabled = true\n[providers.openrouter.routing.max_price]\ntokens = 1.0\n",
		"unknown cache key field": base + "kind = \"chat\"\ncache_key_field = \"user\"\n",
		"unknown data collection": base + "kind = \"chat\"\n[providers.openrouter]\nenabled = true\n[providers.openrouter.routing]\ndata_collection = \"maybe\"\n",
	}
	for name, toml := range cases {
		_, err := loadProviders(t, toml)
		require.Error(t, err, name)
	}
}
