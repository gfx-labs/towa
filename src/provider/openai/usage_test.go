package openai_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/openai"
	"github.com/gfx-labs/towa/src/provider/providertest"
	"github.com/gfx-labs/towa/src/support/httpclient"
)

// The usage of a ChatGPT login, recorded on 2026-11-12: one weekly window.
func TestUsageReadsTheSubscriptionsLimits(t *testing.T) {
	path := filepath.Join("testdata", "codex_usage.json")
	cred := auth.Credential{Account: accountInTest, Scheme: auth.SchemeOAuth, Token: providertest.Redacted, ChatGPTAccountID: providertest.Redacted}
	httpClient := httpclient.New()
	cfg := openai.Config{HTTPClient: httpClient}
	if os.Getenv(recordEnv) != "" {
		cred.Token, cred.ChatGPTAccountID = os.Getenv("TOWA_CODEX_TOKEN"), os.Getenv("TOWA_CODEX_ACCOUNT_ID")
		if cred.Token == "" {
			t.Skip("the credential is not in the environment")
		}
		// The response names the user: TOWA_CODEX_REDACT lists, separated by
		// commas, what else must not reach the recording.
		secrets := append([]string{cred.Token, cred.ChatGPTAccountID}, strings.Split(os.Getenv("TOWA_CODEX_REDACT"), ",")...)
		httpClient.Transport = &providertest.Recorder{Next: httpClient.Transport, Path: path, Secrets: secrets}
	} else {
		if _, err := providertest.Load(path); errors.Is(err, fs.ErrNotExist) {
			t.Skipf("%s has not been recorded", path)
		}
		server := providertest.Replay(t, path)
		cfg.BaseURL, cfg.CodexBaseURL = server.URL+"/v1", server.URL+"/backend-api/codex"
	}
	client, err := openai.New(cfg)
	require.NoError(t, err)

	usage, err := client.Usage(t.Context(), cred)
	require.NoError(t, err)
	require.NotEmpty(t, usage.Plan)
	require.Len(t, usage.Limits, 1)
	require.Equal(t, "7d", usage.Limits[0].Name)
	require.GreaterOrEqual(t, usage.Limits[0].UsedPercent, 0.0)
	require.LessOrEqual(t, usage.Limits[0].UsedPercent, 100.0)
	require.True(t, usage.Limits[0].ResetsAt.After(time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)))

	// An API key has no subscription, and no request is sent for one.
	_, err = client.Usage(t.Context(), auth.Credential{Account: "key", Scheme: auth.SchemeAPIKey, Token: "k"})
	require.ErrorIs(t, err, provider.ErrNoUsage)
}
