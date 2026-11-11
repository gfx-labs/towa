package providertest

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// Option changes what Replay compares.
type Option func(*replayer)

// IgnoreHeader leaves the named request headers out of the comparison, for a
// header whose value changes between runs, such as a user agent that carries
// the build's version.
func IgnoreHeader(names ...string) Option {
	return func(r *replayer) {
		for _, n := range names {
			r.ignore[http.CanonicalHeaderKey(n)] = true
		}
	}
}

// transportHeaders are added by net/http's transport after the recorder saw
// the request, so a recording never has them.
var transportHeaders = map[string]bool{
	"Accept-Encoding":   true,
	"Content-Length":    true,
	"Transfer-Encoding": true,
	"Connection":        true,
}

// goUserAgent is what the transport sends when the client set no user agent.
const goUserAgent = "Go-http-client/"

// responseSkip are response headers the replay server must set itself: the
// recorded body is already decoded and is sent again in its chunks.
var responseSkip = map[string]bool{
	"Content-Length":    true,
	"Content-Encoding":  true,
	"Transfer-Encoding": true,
	"Connection":        true,
}

type replayer struct {
	t      testing.TB
	path   string
	ex     *Exchange
	ignore map[string]bool
	served atomic.Int64
}

// Replay starts a server that answers with the recording at path. Send the
// client's request to the server's URL in place of the provider's base URL:
// the path and query are compared with the recording, the host is not.
//
// The test fails when the request differs from the recorded one: in its
// method, path, query, headers or body. A body that is JSON on both sides is
// compared as JSON, so member order and spacing do not matter. A value the
// recorder replaced with Redacted only has to be present. The test also
// fails when no request reached the server.
//
// A response whose recording ended with a read error is cut off the same
// way: the connection is dropped after the recorded chunks.
func Replay(t testing.TB, path string, opts ...Option) *httptest.Server {
	t.Helper()
	ex, err := Load(path)
	if err != nil {
		t.Fatalf("%v", err)
	}
	r := &replayer{t: t, path: path, ex: ex, ignore: map[string]bool{}}
	for _, o := range opts {
		o(r)
	}
	server := httptest.NewServer(r)
	t.Cleanup(func() {
		server.Close()
		if r.served.Load() == 0 {
			t.Errorf("providertest: no request was sent to the replay of %s", path)
		}
	})
	return server
}

func (r *replayer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.served.Add(1)
	body, err := io.ReadAll(req.Body)
	if err != nil {
		r.t.Errorf("providertest: read request body: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if diffs := r.compare(req, body); len(diffs) > 0 {
		msg := fmt.Sprintf("providertest: request differs from the recording %s:\n  %s", r.path, strings.Join(diffs, "\n  "))
		r.t.Errorf("%s", msg)
		http.Error(w, msg, http.StatusInternalServerError)
		return
	}

	for name, values := range r.ex.Response.Header {
		if responseSkip[http.CanonicalHeaderKey(name)] {
			continue
		}
		w.Header()[name] = values
	}
	w.WriteHeader(r.ex.Response.Status)
	rc := http.NewResponseController(w)
	for _, chunk := range r.ex.Response.Chunks {
		if _, err := w.Write(chunk); err != nil {
			return // the client went away, which a test may do on purpose
		}
		if err := rc.Flush(); err != nil {
			return
		}
	}
	if r.ex.Response.ReadError != "" {
		// Drops the connection without the final chunk, so the client's
		// read fails as it did when the exchange was recorded.
		panic(http.ErrAbortHandler)
	}
}

// compare lists the ways req differs from the recorded request.
func (r *replayer) compare(req *http.Request, body []byte) []string {
	var diffs []string
	want := r.ex.Request
	if req.Method != want.Method {
		diffs = append(diffs, fmt.Sprintf("method: sent %s, recorded %s", req.Method, want.Method))
	}
	wantURL, err := url.Parse(want.URL)
	if err != nil {
		return append(diffs, fmt.Sprintf("recorded url %q: %v", want.URL, err))
	}
	if req.URL.Path != wantURL.Path {
		diffs = append(diffs, fmt.Sprintf("path: sent %s, recorded %s", req.URL.Path, wantURL.Path))
	}
	diffs = append(diffs, compareValues("query", req.URL.Query(), wantURL.Query(), nil)...)

	sent := map[string][]string{}
	for name, values := range req.Header {
		if transportHeaders[name] {
			continue
		}
		_, recorded := want.Header[name]
		if name == "User-Agent" && !recorded && len(values) == 1 && strings.HasPrefix(values[0], goUserAgent) {
			continue
		}
		sent[name] = values
	}
	diffs = append(diffs, compareValues("header", sent, want.Header, r.ignore)...)
	return append(diffs, compareBody(body, want.Body)...)
}

func compareValues(kind string, sent, recorded map[string][]string, ignore map[string]bool) []string {
	names := map[string]bool{}
	for n := range sent {
		names[n] = true
	}
	for n := range recorded {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		if !ignore[n] {
			sorted = append(sorted, n)
		}
	}
	sort.Strings(sorted)

	var diffs []string
	for _, n := range sorted {
		s, hasSent := sent[n]
		rec, hasRec := recorded[n]
		switch {
		case !hasSent:
			diffs = append(diffs, fmt.Sprintf("%s %s: not sent, recorded %q", kind, n, rec))
		case !hasRec:
			// A secret value is not printed, even one from a test.
			if SecretName(n) {
				diffs = append(diffs, fmt.Sprintf("%s %s: sent, not recorded", kind, n))
			} else {
				diffs = append(diffs, fmt.Sprintf("%s %s: sent %q, not recorded", kind, n, s))
			}
		case slices.Equal(rec, redactedValues(len(rec))):
			if len(s) != len(rec) || slices.Contains(s, "") {
				diffs = append(diffs, fmt.Sprintf("%s %s: sent %d values, recorded %d secret values", kind, n, len(s), len(rec)))
			}
		case !slices.Equal(s, rec):
			diffs = append(diffs, fmt.Sprintf("%s %s: sent %q, recorded %q", kind, n, s, rec))
		}
	}
	return diffs
}

func redactedValues(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = Redacted
	}
	return out
}

func compareBody(sent, recorded []byte) []string {
	var s, rec any
	if json.Unmarshal(sent, &s) != nil || json.Unmarshal(recorded, &rec) != nil {
		if !bytes.Equal(sent, recorded) {
			return []string{fmt.Sprintf("body: sent %q, recorded %q", sent, recorded)}
		}
		return nil
	}
	var diffs []string
	diffJSON("body", s, rec, &diffs)
	return diffs
}

// diffJSON appends one line for each place the two decoded JSON values
// differ, named by its path.
func diffJSON(path string, sent, recorded any, diffs *[]string) {
	switch rec := recorded.(type) {
	case map[string]any:
		s, ok := sent.(map[string]any)
		if !ok {
			break
		}
		names := map[string]bool{}
		for n := range s {
			names[n] = true
		}
		for n := range rec {
			names[n] = true
		}
		sorted := make([]string, 0, len(names))
		for n := range names {
			sorted = append(sorted, n)
		}
		sort.Strings(sorted)
		for _, n := range sorted {
			sv, hasSent := s[n]
			rv, hasRec := rec[n]
			switch {
			case !hasSent:
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: not sent, recorded %s", path, n, show(rv)))
			case !hasRec:
				*diffs = append(*diffs, fmt.Sprintf("%s.%s: sent %s, not recorded", path, n, show(sv)))
			default:
				diffJSON(path+"."+n, sv, rv, diffs)
			}
		}
		return
	case []any:
		s, ok := sent.([]any)
		if !ok {
			break
		}
		if len(s) != len(rec) {
			*diffs = append(*diffs, fmt.Sprintf("%s: sent %d elements, recorded %d", path, len(s), len(rec)))
			return
		}
		for i := range rec {
			diffJSON(fmt.Sprintf("%s[%d]", path, i), s[i], rec[i], diffs)
		}
		return
	}
	if !reflect.DeepEqual(sent, recorded) {
		*diffs = append(*diffs, fmt.Sprintf("%s: sent %s, recorded %s", path, show(sent), show(recorded)))
	}
}

func show(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
