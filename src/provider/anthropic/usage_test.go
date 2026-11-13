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

// The usage of a subscription login, recorded on 2026-11-12: a weekly
// window in use, a limit of one model, and no five hour window.
func TestUsageReadsTheSubscriptionsLimits(t *testing.T) {
	path := filepath.Join("testdata", "oauth_usage.json")
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

	usage, err := client.Usage(t.Context(), cred)
	require.NoError(t, err)
	require.Empty(t, usage.Plan)
	require.Len(t, usage.Limits, 2)
	require.Equal(t, "seven_day", usage.Limits[0].Name)
	require.Equal(t, "weekly_scoped Fable", usage.Limits[1].Name)
	for _, limit := range usage.Limits {
		require.GreaterOrEqual(t, limit.UsedPercent, 0.0)
		require.LessOrEqual(t, limit.UsedPercent, 100.0)
		require.False(t, limit.ResetsAt.IsZero())
	}

	// An API key has no subscription, and no request is sent for one.
	_, err = client.Usage(t.Context(), auth.Credential{Account: "key", Scheme: auth.SchemeAPIKey, Token: "k"})
	require.ErrorIs(t, err, provider.ErrNoUsage)
}
