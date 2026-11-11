package embed_test

import (
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gfx-labs/acpharness/src/provider/providertest"
)

func unmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// serve answers every request with the recorded response, without comparing
// the request. It is for a test that sends a request the recording does not
// hold and checks what the client makes of a real answer.
func serve(t *testing.T, ex *providertest.Exchange) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ex.Response.Status)
		_, _ = w.Write(ex.Response.Body())
	}))
	t.Cleanup(server.Close)
	return server
}
