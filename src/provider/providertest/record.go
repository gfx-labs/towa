package providertest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	if r.Path == "" {
		return nil, errors.New("providertest: recording path is empty")
	}
	// Refuse an unusable parent before the endpoint can have an effect.
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return nil, fmt.Errorf("providertest: recording directory: %w", err)
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
	b.ex.Response.Chunks = redactChunks(b.ex.Response.Chunks, secrets)
	// Nothing above should leave a secret. Writing one would leak it, so the
	// recording is refused if it did.
	whole := b.ex.Response.Body()
	for _, secret := range secrets {
		if secret != "" && bytes.Contains(whole, []byte(secret)) {
			return fmt.Errorf("providertest: a secret is left in the response, %s was not written", b.rec.Path)
		}
	}
	return b.ex.write(b.rec.Path)
}

// redactChunks replaces every secret in the body the chunks make up, and
// returns it in the same number of chunks. A secret can lie across two
// reads, so it is looked for in the whole body. A chunk boundary inside a
// secret moves to the end of what replaced it. Every other boundary stays
// where it was.
func redactChunks(chunks []Bytes, secrets []string) []Bytes {
	var whole []byte
	ends := make([]int, len(chunks))
	for i, c := range chunks {
		whole = append(whole, c...)
		ends[i] = len(whole)
	}
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		var out []byte
		moved := make([]int, len(ends))
		at, next := 0, 0
		for {
			found := bytes.Index(whole[at:], []byte(secret))
			stop := len(whole)
			if found >= 0 {
				stop = at + found
			}
			for next < len(ends) && ends[next] <= stop {
				moved[next] = len(out) + ends[next] - at
				next++
			}
			out = append(out, whole[at:stop]...)
			if found < 0 {
				break
			}
			out = append(out, Redacted...)
			at = stop + len(secret)
			for next < len(ends) && ends[next] < at {
				moved[next] = len(out)
				next++
			}
		}
		whole, ends = out, moved
	}
	redacted := make([]Bytes, len(chunks))
	start := 0
	for i, end := range ends {
		redacted[i] = Bytes(bytes.Clone(whole[start:end]))
		start = end
	}
	return redacted
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
