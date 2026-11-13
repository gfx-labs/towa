package extras_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gfx-labs/towa/src/agent/extras"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/support/httpclient"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/transcript"
	"github.com/stretchr/testify/require"
)

// The pages under ../websearch/testdata are DuckDuckGo's own answers,
// recorded on 2026-11-11: results.html to the query "golang
// encoding/json/v2 omitzero", and challenge.html, with status 202, to a
// burst of searches.
func TestWebSearchReadsARecordedResultPageAndReportsAChallenge(t *testing.T) {
	var form string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Method != http.MethodPost {
			http.Error(w, "the search page takes a posted form", http.StatusBadRequest)
			return
		}
		form = r.PostForm.Encode()
		if r.URL.Path == "/challenge" {
			w.WriteHeader(http.StatusAccepted)
			http.ServeFile(w, r, "../../tools/websearch/testdata/challenge.html")
			return
		}
		http.ServeFile(w, r, "../../tools/websearch/testdata/results.html")
	}))
	defer srv.Close()
	cfg := config.WebSearch{
		Enabled: true, Endpoint: srv.URL + "/html/", Region: "us-en", Timeout: time.Minute,
		MaxBodyBytes: 1 << 20, DefaultResults: 3, MaxResults: 10, UserAgent: "towa-test",
	}
	tool, err := extras.WebSearch(cfg, httpclient.New())
	require.NoError(t, err)

	res := run(t, tool, `{"query":"golang encoding/json/v2 omitzero"}`, tools.Env{})
	require.Equal(t, transcript.OutcomeCompleted, res.Outcome)
	require.Equal(t, "kl=us-en&q=golang+encoding%2Fjson%2Fv2+omitzero", form)
	got := text(res)
	// A title's entity is decoded and a snippet's markup is dropped.
	require.Contains(t, got, "1. Golang JSON omitempty: omitzero & Nested Structs - GoLinuxCloud\n   https://www.golinuxcloud.com/golang-json-omitempty/\n   Learn how Go JSON omitempty works")
	require.Contains(t, got, "how omitzero differs, and what changes with encoding/json/v2 in Go 1.27.")
	require.Contains(t, got, "\n3. ")
	require.NotContains(t, got, "\n4. ")

	// The page holds ten results, and a call may ask for all of them.
	res = run(t, tool, `{"query":"golang","num_results":10}`, tools.Env{})
	require.Equal(t, 10, strings.Count(text(res), "\n   http"))

	for _, input := range []string{`{"query":"  "}`, `{"query":"golang","num_results":11}`, `{"query":"golang","num_results":-1}`} {
		require.Equal(t, transcript.OutcomeFailed, run(t, tool, input, tools.Env{}).Outcome, input)
	}

	cfg.Endpoint = srv.URL + "/challenge"
	tool, err = extras.WebSearch(cfg, httpclient.New())
	require.NoError(t, err)
	res = run(t, tool, `{"query":"golang"}`, tools.Env{})
	require.Equal(t, transcript.OutcomeFailed, res.Outcome)
	require.Contains(t, text(res), "bot challenge")
}
