package looptest

import (
	"context"
	"net/http"
	"os"
	"testing"
	"uuid"

	"github.com/stretchr/testify/require"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/pkg/harness"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/provider/providertest"
)

// TestRecord runs each scenario's turn against the Ollama server and writes
// one recording per model request. Run it by hand:
//
//	TOWA_RECORD_OLLAMA=1 go test ./src/agent/loop/looptest -run TestRecord -count=1
//
// A turn that makes another number of requests than its scenario says
// fails, and its recordings are not to be kept: the model did not do what
// the scenario describes.
func TestRecord(t *testing.T) {
	if os.Getenv(EnvRecord) == "" {
		t.Skip(EnvRecord + " is not set")
	}
	for _, s := range []Scenario{ReadTurn, UnknownModel} {
		t.Run(s.Name, func(t *testing.T) {
			f := build(t, s, OllamaBase, func(next http.RoundTripper) *sequence {
				seq := &sequence{}
				for n := range s.Requests {
					seq.recorders = append(seq.recorders, &providertest.Recorder{Next: next, Path: RecordingPath(s, n+1)})
				}
				return seq
			})
			turn := f.Turn(acp.SessionId(uuid.NewV7().String()), func(context.Context, acp.SessionUpdate) error { return nil })
			result, err := f.Loop.Run(t.Context(), turn)
			t.Logf("result %+v, error %v", result, err)
			require.Equal(t, s.Requests, f.Served(), "the turn made another number of requests than the scenario says")
			if s.Name == ReadTurn.Name {
				require.NoError(t, err)
				require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
				require.Contains(t, result.Final, "7421")
			} else {
				var stopped *loop.StopError
				require.ErrorAs(t, err, &stopped)
				require.Equal(t, harness.StopRequest, stopped.Stop.Kind)
			}
		})
	}
}
