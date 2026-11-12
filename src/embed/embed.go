// Package embed is the client for an embeddings endpoint that speaks the
// OpenAI /v1/embeddings shape: OpenAI itself, or a sidecar such as Text
// Embeddings Inference, Ollama or llama-server.
package embed

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
)

// ErrDisabled says the configuration names no embeddings endpoint.
var ErrDisabled = errors.New("embeddings.base_url is empty, so there is no embeddings endpoint")

// Error is a request the endpoint refused.
type Error struct {
	// Status is the HTTP status.
	Status int
	// Type, Code and Message are the members of the answer's "error"
	// object. They are empty when the answer was not that shape. Code is
	// the raw JSON value, since endpoints send a string, a number or null.
	Type    string
	Code    jsontext.Value
	Message string
	// RetryAfter is how long the endpoint asked the caller to wait, from a
	// Retry-After header in seconds. It is zero when there was none.
	RetryAfter time.Duration
}

func (e *Error) Error() string {
	text := "embeddings endpoint answered " + strconv.Itoa(e.Status)
	if e.Type != "" {
		text += " (" + e.Type + ")"
	}
	if e.Message != "" {
		text += ": " + e.Message
	}
	return text
}

// ResponseError is an answer with status 200 that is not a usable result:
// it does not hold one vector per input, or a vector has the wrong length.
type ResponseError struct {
	Problem string
}

func (e *ResponseError) Error() string { return "embeddings response: " + e.Problem }

// Options is what a Client is built from.
type Options struct {
	Config config.Embeddings
	// HTTPClient sends the requests. It comes from src/httpclient.
	HTTPClient *http.Client
	// Credentials hands out the credential of Config.Account. It is
	// required when Config.Account is set and not used otherwise.
	Credentials auth.Source
}

// Client sends inputs to the configured endpoint and returns their vectors.
type Client struct {
	opts Options
	url  string
}

// New returns the client for the configured endpoint. Without one the error
// is ErrDisabled.
func New(opts Options) (*Client, error) {
	cfg := opts.Config
	switch {
	case cfg.BaseURL == "":
		return nil, ErrDisabled
	case opts.HTTPClient == nil:
		return nil, errors.New("embed: Options.HTTPClient is nil")
	case cfg.Account != "" && opts.Credentials == nil:
		return nil, fmt.Errorf("embed: embeddings.account is %q and Options.Credentials is nil", cfg.Account)
	case cfg.Model == "":
		return nil, errors.New("embed: embeddings.model is empty")
	case cfg.BatchSize <= 0:
		return nil, fmt.Errorf("embed: embeddings.batch_size is %d", cfg.BatchSize)
	case cfg.TimeoutSeconds <= 0:
		return nil, fmt.Errorf("embed: embeddings.timeout_seconds is %d", cfg.TimeoutSeconds)
	}
	return &Client{opts: opts, url: strings.TrimSuffix(cfg.BaseURL, "/") + "/embeddings"}, nil
}

type request struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitzero"`
}

type response struct {
	Data []struct {
		// Index is a pointer so that a missing index is told from 0.
		Index     *int      `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per input, in the order of the inputs. The
// inputs are sent in requests of at most embeddings.batch_size each, one
// after another. An error from any request fails the whole call, and no
// vector is returned.
//
// A refusal is an *Error, an unusable answer a *ResponseError, and a failed
// credential the auth package's own error. Anything else is the network.
func (c *Client) Embed(ctx context.Context, inputs []string) ([][]float32, error) {
	vectors := make([][]float32, 0, len(inputs))
	for start := 0; start < len(inputs); start += c.opts.Config.BatchSize {
		end := min(start+c.opts.Config.BatchSize, len(inputs))
		batch, err := c.embedBatch(ctx, inputs[start:end])
		if err != nil {
			return nil, fmt.Errorf("embed inputs %d to %d of %d: %w", start, end-1, len(inputs), err)
		}
		vectors = append(vectors, batch...)
	}
	// Every vector of one call has one length, across its requests too.
	for i, v := range vectors {
		if len(v) != len(vectors[0]) {
			return nil, &ResponseError{Problem: fmt.Sprintf("the vector of input %d has %d numbers and that of input 0 has %d", i, len(v), len(vectors[0]))}
		}
	}
	return vectors, nil
}

func (c *Client) embedBatch(ctx context.Context, inputs []string) ([][]float32, error) {
	cfg := c.opts.Config
	body, err := json.Marshal(request{Model: cfg.Model, Input: inputs, Dimensions: cfg.Dimensions})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if cfg.Account != "" {
		cred, err := c.opts.Credentials.Credential(ctx, cfg.Account)
		if err != nil {
			return nil, err
		}
		// An account of kind none has no token to send.
		if cred.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cred.Token)
		}
	}
	resp, err := c.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about it

	if resp.StatusCode != http.StatusOK {
		return nil, refusal(resp)
	}
	var got response
	if err := json.UnmarshalRead(resp.Body, &got); err != nil {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, fmt.Errorf("embeddings response: %w", err)
	}
	if len(got.Data) != len(inputs) {
		return nil, &ResponseError{Problem: fmt.Sprintf("%d vectors for %d inputs", len(got.Data), len(inputs))}
	}
	vectors := make([][]float32, len(inputs))
	for _, item := range got.Data {
		switch {
		case item.Index == nil:
			return nil, &ResponseError{Problem: "a vector has no index"}
		case *item.Index < 0 || *item.Index >= len(inputs):
			return nil, &ResponseError{Problem: fmt.Sprintf("index %d for %d inputs", *item.Index, len(inputs))}
		case vectors[*item.Index] != nil:
			return nil, &ResponseError{Problem: fmt.Sprintf("two vectors have index %d", *item.Index)}
		case len(item.Embedding) == 0:
			return nil, &ResponseError{Problem: fmt.Sprintf("the vector of index %d is empty", *item.Index)}
		case cfg.Dimensions != 0 && len(item.Embedding) != cfg.Dimensions:
			return nil, &ResponseError{Problem: fmt.Sprintf("a vector has %d numbers and embeddings.dimensions is %d", len(item.Embedding), cfg.Dimensions)}
		}
		vectors[*item.Index] = item.Embedding
	}
	return vectors, nil
}

// refusal reads the endpoint's error answer.
func refusal(resp *http.Response) *Error {
	e := &Error{Status: resp.StatusCode}
	if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
		e.RetryAfter = time.Duration(seconds) * time.Second
	}
	var body struct {
		Error struct {
			Type    string         `json:"type"`
			Code    jsontext.Value `json:"code"`
			Message string         `json:"message"`
		} `json:"error"`
	}
	// An answer of another shape, such as a proxy's page, is reported by
	// its status alone.
	if json.UnmarshalRead(resp.Body, &body) == nil {
		e.Type, e.Code, e.Message = body.Error.Type, body.Error.Code, body.Error.Message
	}
	return e
}
