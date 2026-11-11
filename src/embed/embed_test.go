package embed_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/embed"
	"github.com/gfx-labs/acpharness/src/httpclient"
	"github.com/gfx-labs/acpharness/src/provider/providertest"
)

// The recordings are of an Ollama server answering with a chat model. They
// show the wire shape and nothing of OpenAI's own behaviour.
const (
	recordedOK      = "testdata/embeddings_ok.json"
	recordedRefused = "testdata/embeddings_unknown_model.json"
	// The variables that make TestRecord run against a live endpoint.
	envRecordURL   = "ACPHARNESS_EMBED_RECORD_URL"
	envRecordModel = "ACPHARNESS_EMBED_RECORD_MODEL"
	// The inputs and the model names are part of the recorded requests.
	recordedModel = "llama3.2:3b"
	unknownModel  = "acpharness-no-such-model"
)

var recordedInputs = []string{"a red bicycle", "the sea at night"}

func client(t *testing.T, baseURL, model string, transport http.RoundTripper) *embed.Client {
	hc := httpclient.New()
	if transport != nil {
		hc.Transport = transport
	}
	c, err := embed.New(embed.Options{
		Config:     config.Embeddings{BaseURL: baseURL, Model: model, BatchSize: 32, TimeoutSeconds: 120},
		HTTPClient: hc,
	})
	require.NoError(t, err)
	return c
}

// TestRecord writes the recordings from a live endpoint. It runs only when
// both variables are set, and it sends two requests.
func TestRecord(t *testing.T) {
	baseURL, model := os.Getenv(envRecordURL), os.Getenv(envRecordModel)
	if baseURL == "" || model == "" {
		t.Skipf("%s and %s are not both set", envRecordURL, envRecordModel)
	}
	require.Equal(t, recordedModel, model, "the replay tests send this model")
	require.NoError(t, os.MkdirAll(filepath.Dir(recordedOK), 0o755))

	ok := &providertest.Recorder{Next: httpclient.Transport(), Path: recordedOK}
	vectors, err := client(t, baseURL, model, ok).Embed(t.Context(), recordedInputs)
	require.NoError(t, err)
	require.Len(t, vectors, len(recordedInputs))

	refused := &providertest.Recorder{Next: httpclient.Transport(), Path: recordedRefused}
	_, err = client(t, baseURL, unknownModel, refused).Embed(t.Context(), recordedInputs[:1])
	var refusal *embed.Error
	require.ErrorAs(t, err, &refusal)
}

// Two inputs in one request come back as two different vectors of one
// length, in the order of the inputs.
func TestEmbedReplaysARecordedAnswer(t *testing.T) {
	server := providertest.Replay(t, recordedOK)
	vectors, err := client(t, server.URL+"/v1", recordedModel, nil).Embed(t.Context(), recordedInputs)
	require.NoError(t, err)
	require.Len(t, vectors, 2)
	require.NotEmpty(t, vectors[0])
	require.Len(t, vectors[1], len(vectors[0]))
	require.NotEqual(t, vectors[0], vectors[1])

	// The recording says which vector belongs to which input.
	ex, err := providertest.Load(recordedOK)
	require.NoError(t, err)
	var recorded struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	require.NoError(t, unmarshal(ex.Response.Body(), &recorded))
	for _, item := range recorded.Data {
		require.Equal(t, item.Embedding, vectors[item.Index])
	}
}

// The same answer with a length configured that the model does not give.
func TestEmbedRefusesAVectorOfAnotherLength(t *testing.T) {
	ex, err := providertest.Load(recordedOK)
	require.NoError(t, err)
	// The request now carries the dimensions, so it is not compared with
	// the recording: the recorded answer is served as it is.
	server := serve(t, ex)
	c, err := embed.New(embed.Options{
		Config:     config.Embeddings{BaseURL: server.URL + "/v1", Model: recordedModel, Dimensions: 8, BatchSize: 32, TimeoutSeconds: 120},
		HTTPClient: httpclient.New(),
	})
	require.NoError(t, err)
	_, err = c.Embed(t.Context(), recordedInputs)
	var unusable *embed.ResponseError
	require.ErrorAs(t, err, &unusable)

	// One input for an answer that holds two vectors.
	_, err = client(t, server.URL+"/v1", recordedModel, nil).Embed(t.Context(), recordedInputs[:1])
	require.ErrorAs(t, err, &unusable)
}

func TestEmbedReportsARecordedRefusal(t *testing.T) {
	server := providertest.Replay(t, recordedRefused)
	_, err := client(t, server.URL+"/v1", unknownModel, nil).Embed(t.Context(), recordedInputs[:1])
	var refusal *embed.Error
	require.ErrorAs(t, err, &refusal)

	ex, loadErr := providertest.Load(recordedRefused)
	require.NoError(t, loadErr)
	var recorded struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	require.NoError(t, unmarshal(ex.Response.Body(), &recorded))
	require.Equal(t, ex.Response.Status, refusal.Status)
	require.NotEmpty(t, recorded.Error.Message)
	require.Equal(t, recorded.Error.Message, refusal.Message)
	require.Equal(t, recorded.Error.Type, refusal.Type)
}

func TestNoEndpointConfigured(t *testing.T) {
	_, err := embed.New(embed.Options{HTTPClient: httpclient.New()})
	require.ErrorIs(t, err, embed.ErrDisabled)
}
