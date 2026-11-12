// Package providertest records one real HTTP exchange with a provider to a
// file and replays it. A provider test replays a recording through the real
// client over real HTTP, so the client's request building, its stream
// parsing and its error handling are all exercised against what the provider
// sent.
//
// To record, wrap the transport of a client from src/httpclient:
//
//	client := httpclient.New()
//	client.Transport = &providertest.Recorder{Next: client.Transport, Path: path, Secrets: []string{key}}
//
// To replay, point the client at the server Replay returns:
//
//	server := providertest.Replay(t, path)
//	// send the same request to server.URL with a client from src/httpclient
package providertest

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"
)

// Redacted replaces a secret in a recording.
const Redacted = "REDACTED"

// Exchange is one recorded request and its response. It is the content of a
// recording file.
type Exchange struct {
	Request  Request  `json:"request"`
	Response Response `json:"response"`
}

// Request is the request the client sent, as the transport received it.
type Request struct {
	Method string `json:"method"`
	// URL is the full URL the request went to.
	URL    string              `json:"url"`
	Header map[string][]string `json:"header"`
	Body   Bytes               `json:"body,omitzero"`
}

// Response is what the provider answered.
type Response struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header"`
	// Chunks is the body in the pieces the client read it in, after the
	// transport removed any content encoding.
	Chunks []Bytes `json:"chunks"`
	// Incomplete says the recording stopped before the end of the body:
	// the client closed it early, or reading it failed with ReadError.
	Incomplete bool   `json:"incomplete,omitzero"`
	ReadError  string `json:"read_error,omitzero"`
}

// Body returns the chunks joined.
func (r *Response) Body() []byte {
	var out []byte
	for _, c := range r.Chunks {
		out = append(out, c...)
	}
	return out
}

// Bytes is body content. It is written as {"text": ...} when it is valid
// UTF-8, so that a recording can be read and reviewed, and as
// {"base64": ...} otherwise.
type Bytes []byte

type bytesJSON struct {
	Text   *string `json:"text,omitzero"`
	Base64 []byte  `json:"base64,omitzero"`
}

// MarshalJSONTo writes b in its text or base64 form.
func (b Bytes) MarshalJSONTo(enc *jsontext.Encoder) error {
	if utf8.Valid(b) {
		s := string(b)
		return json.MarshalEncode(enc, bytesJSON{Text: &s})
	}
	return json.MarshalEncode(enc, bytesJSON{Base64: b})
}

// UnmarshalJSONFrom reads either form.
func (b *Bytes) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var v bytesJSON
	if err := json.UnmarshalDecode(dec, &v, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	switch {
	case v.Text != nil && v.Base64 != nil:
		return fmt.Errorf("providertest: body has both text and base64")
	case v.Text != nil:
		*b = Bytes(*v.Text)
	default:
		*b = Bytes(v.Base64)
	}
	return nil
}

// Load reads a recording file.
func Load(path string) (*Exchange, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("providertest: %w", err)
	}
	var ex Exchange
	if err := json.Unmarshal(data, &ex, json.RejectUnknownMembers(true)); err != nil {
		return nil, fmt.Errorf("providertest: %s: %w", path, err)
	}
	if ex.Request.Method == "" || ex.Request.URL == "" || ex.Response.Status == 0 {
		return nil, fmt.Errorf("providertest: %s: not a recording: method, url or status is missing", path)
	}
	return &ex, nil
}

func (ex *Exchange) write(path string) error {
	data, err := json.Marshal(ex, json.Deterministic(true), jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("providertest: %w", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil { //nolint:gosec // a recording is a test fixture committed to the repo
		return fmt.Errorf("providertest: %w", err)
	}
	return nil
}

// secretWords mark a header or query parameter as holding a credential, or
// an id of the account that recorded, when its name contains one of them.
// A recording is committed, so it must not name whose account made it.
var secretWords = []string{
	"auth", "cookie", "key", "token", "secret", "password", "credential", "session", "signature",
	"organization", "workspace", "account",
}

// SecretName reports whether a header or query parameter with this name is
// replaced in a recording.
func SecretName(name string) bool {
	name = strings.ToLower(name)
	for _, w := range secretWords {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// redactHeader copies h with the values of secret headers and every listed
// secret value replaced.
func redactHeader(h http.Header, secrets []string) map[string][]string {
	out := make(map[string][]string, len(h))
	for name, values := range h {
		copied := make([]string, len(values))
		for i, v := range values {
			if SecretName(name) {
				copied[i] = Redacted
			} else {
				copied[i] = redactString(v, secrets)
			}
		}
		out[name] = copied
	}
	return out
}

func redactString(s string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			s = strings.ReplaceAll(s, secret, Redacted)
		}
	}
	return s
}
