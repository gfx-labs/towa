package anthropic_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/anthropic"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/support/httpclient"
)

// The model list of a subscription login, recorded on 2026-11-12.
func TestModelsReadsTheAccountsList(t *testing.T) {
	path := filepath.Join("testdata", "oauth_models.json")
	cred := auth.Credential{Account: "recorded", Scheme: auth.SchemeOAuth, Token: providertest.Redacted}
	httpClient := httpclient.New()
	var baseURL string
	if os.Getenv(recordEnv) != "" {
		cred.Token = os.Getenv("TOWA_CLAUDE_OAUTH_TOKEN")
		if cred.Token == "" {
			t.Skip("the credential is not in the environment")
		}
		httpClient.Transport = &providertest.Recorder{Next: httpClient.Transport, Path: path, Secrets: []string{cred.Token}}
	} else {
		if _, err := providertest.Load(path); errors.Is(err, fs.ErrNotExist) {
			t.Skipf("%s has not been recorded", path)
		}
		baseURL = providertest.Replay(t, path, providertest.IgnoreHeader("X-Client-Request-Id")).URL
	}
	client, err := anthropic.New(anthropic.Config{HTTPClient: httpClient, Version: version, BaseURL: baseURL})
	require.NoError(t, err)

	list, err := client.Models(t.Context(), cred)
	require.NoError(t, err)
	byID := map[string]provider.ModelInfo{}
	for _, m := range list {
		byID[m.ID] = m
	}
	require.Len(t, byID, len(list), "no model is listed twice")
	require.Equal(t, provider.ModelInfo{ID: "claude-opus-5-5", ContextWindow: 1_000_000, MaxOutputTokens: 128_000}, byID["claude-opus-5-5"])
	// An older model is listed with its release date, a smaller window, and
	// adaptive thinking marked as not supported.
	require.Equal(t, provider.ModelInfo{
		ID: "claude-opus-4-5-20251101", ContextWindow: 200_000, MaxOutputTokens: 64_000, NoAdaptiveThinking: true,
	}, byID["claude-opus-4-5-20251101"])
}
