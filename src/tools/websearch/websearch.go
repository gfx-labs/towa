// Package websearch searches the web through DuckDuckGo's HTML page and
// returns the results as titles, URLs and snippets. The search is made by
// this process, so it works the same with every model provider. It is one
// capability with typed input and a typed result. The tool that calls it
// turns a result or an *Error into the text the model reads.
package websearch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"

	"github.com/gfx-labs/towa/src/config"
)

// Kind says why a search was refused or failed in a way the model can act
// on.
type Kind string

// Kinds of an Error.
const (
	// KindEmptyQuery: there is no query.
	KindEmptyQuery Kind = "empty_query"
	// KindCount: the number of results asked for is negative or over
	// websearch.max_results. Limit holds that maximum.
	KindCount Kind = "count"
	// KindTimeout: the search took longer than websearch.timeout. Limit
	// holds the timeout in nanoseconds.
	KindTimeout Kind = "timeout"
	// KindNetwork: no response arrived, or the body was cut off.
	KindNetwork Kind = "network"
	// KindStatus: the server answered with a status that is not 2xx.
	KindStatus Kind = "status"
	// KindTooLarge: the page is over websearch.max_body_bytes, in Limit.
	KindTooLarge Kind = "too_large"
	// KindBlocked: the server answered with its bot challenge and no
	// results. It does this to an address that has searched too often.
	KindBlocked Kind = "blocked"
	// KindParse: the page could not be parsed as HTML.
	KindParse Kind = "parse"
)

// Error is a failure the model caused or can act on. Any other error from
// this package means the call was cancelled or the harness could not search.
type Error struct {
	Kind   Kind
	Status int
	Limit  int64
	Err    error
}

func (e *Error) Error() string {
	s := "websearch: " + string(e.Kind)
	if e.Kind == KindStatus {
		s += fmt.Sprintf(": HTTP %d", e.Status)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Input is one search.
type Input struct {
	Query string
	// Results is the most results wanted. Zero is websearch.default_results.
	Results int
}

// Result is one search result.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// challengeMarker is in the page DuckDuckGo serves in place of results when
// it takes the client for a bot, as recorded in testdata/challenge.html.
const challengeMarker = "anomaly-modal"

var errTimeout = errors.New("websearch: timeout")

// Search asks the configured endpoint for a query's results. A search that
// finds nothing returns no results and no error. A cancelled ctx returns
// ctx's error, not an *Error.
func Search(ctx context.Context, client *http.Client, cfg config.WebSearch, in Input) ([]Result, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, &Error{Kind: KindEmptyQuery}
	}
	count := in.Results
	if count == 0 {
		count = cfg.DefaultResults
	}
	if count < 0 || count > cfg.MaxResults {
		return nil, &Error{Kind: KindCount, Limit: int64(cfg.MaxResults)}
	}

	searchCtx, cancel := context.WithTimeoutCause(ctx, cfg.Timeout, errTimeout)
	defer cancel()
	transportError := func(err error) error {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("websearch: %w", context.Cause(ctx))
		case errors.Is(context.Cause(searchCtx), errTimeout):
			return &Error{Kind: KindTimeout, Limit: int64(cfg.Timeout)}
		default:
			return &Error{Kind: KindNetwork, Err: err}
		}
	}

	// The page's own form is a POST. On 2026-11-11 a GET of the same query
	// was answered with the bot challenge.
	form := url.Values{"q": {query}, "kl": {cfg.Region}}
	req, err := http.NewRequestWithContext(searchCtx, http.MethodPost, cfg.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("websearch: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html")
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, transportError(err)
	}
	defer resp.Body.Close() //nolint:errcheck // the request body is already sent
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &Error{Kind: KindStatus, Status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBodyBytes+1))
	if err != nil {
		return nil, transportError(err)
	}
	if int64(len(body)) > cfg.MaxBodyBytes {
		return nil, &Error{Kind: KindTooLarge, Limit: cfg.MaxBodyBytes}
	}
	results, err := parse(body, resp.Request.URL, count)
	if err != nil {
		return nil, &Error{Kind: KindParse, Err: err}
	}
	// The challenge comes with status 202, which is a success, so it is
	// told apart by its markup.
	if len(results) == 0 && bytes.Contains(body, []byte(challengeMarker)) {
		return nil, &Error{Kind: KindBlocked}
	}
	return results, nil
}

// parse reads the results out of a result page: each has a link of class
// result__a, and after it a snippet of class result__snippet.
func parse(page []byte, base *url.URL, count int) ([]Result, error) {
	root, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return nil, err
	}
	var results []Result
	for n := range root.Descendants() {
		if n.Type != html.ElementNode || n.DataAtom != atom.A {
			continue
		}
		switch {
		case hasClass(n, "result__a"):
			if len(results) == count {
				return results, nil
			}
			target := link(base, attr(n, "href"))
			if target == "" {
				continue
			}
			results = append(results, Result{Title: text(n), URL: target})
		case hasClass(n, "result__snippet") && len(results) > 0 && results[len(results)-1].Snippet == "":
			results[len(results)-1].Snippet = text(n)
		}
	}
	return results, nil
}

// link returns a result's address, or "" when it is not an http address.
// DuckDuckGo sometimes writes a result as its own redirect, with the
// address in the uddg parameter.
func link(base *url.URL, href string) string {
	u, err := base.Parse(href)
	if err != nil {
		return ""
	}
	if target := u.Query().Get("uddg"); target != "" && u.Path == "/l/" {
		if u, err = url.Parse(target); err != nil {
			return ""
		}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.String()
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasClass(n *html.Node, class string) bool {
	for name := range strings.FieldsSeq(attr(n, "class")) {
		if name == class {
			return true
		}
	}
	return false
}

// text returns an element's text with each run of white space as one space.
func text(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}
