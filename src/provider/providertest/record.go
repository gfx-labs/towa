package providertest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
)

// Recorder is an http.RoundTripper that sends a request through Next and
// writes the exchange to Path once the response body has been read to its
// end or closed. It records one exchange: a second request is an error, so
// that a recording is never overwritten by a retry.
//
// Headers and query parameters whose name SecretName matches are replaced
// with Redacted, and so is every occurrence of a value in Secrets anywhere
// in the exchange. Pass the key in Secrets as well, because a provider may
// echo it in a body.
type Recorder struct {
	Next    http.RoundTripper
	Path    string
	Secrets []string

	mu   sync.Mutex
	used bool
}

// RoundTrip sends req and records it with its response.
func (r *Recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	used := r.used
	r.used = true
	r.mu.Unlock()
	if used {
		return nil, fmt.Errorf("providertest: recorder for %s already recorded a request", r.Path)
	}

	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, fmt.Errorf("providertest: read request body: %w", err)
		}
		if err := req.Body.Close(); err != nil {
			return nil, fmt.Errorf("providertest: close request body: %w", err)
		}
		// RoundTrip must not modify the caller's request.
		req = req.Clone(req.Context())
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	ex := &Exchange{Request: Request{
		Method: req.Method,
		URL:    redactURL(req.URL, r.Secrets),
		Header: redactHeader(req.Header, r.Secrets),
		Body:   Bytes(redactString(string(body), r.Secrets)),
	}}

	resp, err := r.Next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	ex.Response.Status = resp.StatusCode
	ex.Response.Header = redactHeader(resp.Header, r.Secrets)
	resp.Body = &recordingBody{body: resp.Body, rec: r, ex: ex}
	return resp, nil
}

// recordingBody keeps each piece the client reads and writes the recording
// when the body ends.
type recordingBody struct {
	body    io.ReadCloser
	rec     *Recorder
	ex      *Exchange
	written bool
}

func (b *recordingBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if n > 0 {
		b.ex.Response.Chunks = append(b.ex.Response.Chunks, bytes.Clone(p[:n]))
	}
	if err == nil {
		return n, nil
	}
	if !errors.Is(err, io.EOF) {
		b.ex.Response.Incomplete = true
		b.ex.Response.ReadError = err.Error()
	}
	if werr := b.finish(); werr != nil {
		return n, werr
	}
	return n, err
}

func (b *recordingBody) Close() error {
	if !b.written {
		b.ex.Response.Incomplete = true
	}
	werr := b.finish()
	if err := b.body.Close(); err != nil {
		return err
	}
	return werr
}

func (b *recordingBody) finish() error {
	if b.written {
		return nil
	}
	b.written = true
	secrets := b.rec.Secrets
	for i, c := range b.ex.Response.Chunks {
		b.ex.Response.Chunks[i] = Bytes(redactString(string(c), secrets))
	}
	// A secret split across two reads is not replaced above. Writing it
	// would leak it, so the recording is refused.
	whole := b.ex.Response.Body()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(whole, []byte(secret)) {
			return fmt.Errorf("providertest: a secret spans two chunks of the response, %s was not written", b.rec.Path)
		}
	}
	return b.ex.write(b.rec.Path)
}

func redactURL(u *url.URL, secrets []string) string {
	c := *u
	c.User = nil
	q := c.Query()
	for name := range q {
		if SecretName(name) {
			q[name] = []string{Redacted}
		}
	}
	c.RawQuery = q.Encode()
	return redactString(c.String(), secrets)
}
