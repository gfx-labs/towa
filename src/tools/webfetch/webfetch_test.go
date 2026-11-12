package webfetch_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/httpclient"
	"github.com/gfx-labs/towa/src/tools/webfetch"
)

// limits are far above what any fixture needs, so that a test of one limit
// sets only that one.
func limits() config.WebFetch {
	return config.WebFetch{Timeout: time.Minute, MaxBodyBytes: 1 << 20, MaxResultBytes: 1 << 20, UserAgent: "towa-test"}
}

// site serves the fixtures under testdata as a web server would, and the
// handlers the tests add.
func site(t *testing.T, extra map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle("/posts/", http.StripPrefix("/posts/", http.FileServer(http.Dir("testdata"))))
	for pattern, handler := range extra {
		mux.HandleFunc(pattern, handler)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func fetch(t *testing.T, cfg config.WebFetch, url string) (webfetch.Result, error) {
	t.Helper()
	return webfetch.Fetch(t.Context(), httpclient.New(), cfg, webfetch.Input{URL: url})
}

// The Markdown itself is the library's. What is checked is what this package
// decides before the conversion: the title, and where links point.
func TestAnHTMLPageComesBackWithItsTitleAndAbsoluteLinks(t *testing.T) {
	srv := site(t, nil)
	for _, c := range []struct {
		name, title   string
		holds, absent []string
	}{
		// A link to a place in the same page keeps its text and loses its target.
		{"article", "Rotating log files by hand", []string{"](" + srv.URL + "/authors/mira)", "or jump further down."}, []string{"#truncate"}},
		// Links follow the page's base element, and an image that cannot be
		// linked leaves its description.
		{"untidy", "Untidy page & friends", []string{"](https://docs.example.net/guide/v2/install.html)", "A data image: inline pixels."}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			url := srv.URL + "/posts/" + c.name + ".html"
			res, err := fetch(t, limits(), url)
			require.NoError(t, err)
			for _, text := range c.holds {
				require.Contains(t, res.Text, text)
			}
			for _, text := range c.absent {
				require.NotContains(t, res.Text, text)
			}
			require.Equal(t, c.title, res.Title)
			require.Equal(t, webfetch.FormatMarkdown, res.Format)
			require.Equal(t, "text/html", res.ContentType)
			require.Equal(t, url, res.URL)
			require.Equal(t, http.StatusOK, res.Status)
			require.Zero(t, res.OmittedBytes)
		})
	}
}

// What a reader of the page would not see must not reach the model.
func TestScriptsStylesNavigationAndControlsAreDropped(t *testing.T) {
	srv := site(t, nil)
	for name, absent := range map[string][]string{
		"article": {
			"TRACKED", "font-family", "Home", "Archive", "Draft paragraph", "Tracking pixel",
			"Copy command", "Related", "Enable scripts", "javascript:",
		},
		"untidy": {
			"SVG TEXT", "FRAME FALLBACK", "INPUT VALUE", "OPTION TEXT", "TEXTAREA TEXT", "TEMPLATE TEXT",
			"COMMENT TEXT", "ARIA HIDDEN TEXT", "data:image",
		},
	} {
		res, err := fetch(t, limits(), srv.URL+"/posts/"+name+".html")
		require.NoError(t, err)
		for _, text := range absent {
			require.NotContains(t, res.Text, text, name)
		}
	}
}

func TestARedirectIsFollowedAndTheFinalURLReported(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/old/page": func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/posts/article.html", http.StatusMovedPermanently)
		},
	})
	res, err := fetch(t, limits(), srv.URL+"/old/page")
	require.NoError(t, err)
	require.Equal(t, srv.URL+"/posts/article.html", res.URL)
	// Relative links are resolved against where the page was, not against
	// the URL that was asked for.
	require.Contains(t, res.Text, "]("+srv.URL+"/posts/reference.html#signals)")

	t.Run("a redirect loop is a network error", func(t *testing.T) {
		loop := site(t, map[string]http.HandlerFunc{
			"/loop": func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/loop", http.StatusFound) },
		})
		_, err := fetch(t, limits(), loop.URL+"/loop")
		var ferr *webfetch.Error
		require.ErrorAs(t, err, &ferr)
		require.Equal(t, webfetch.KindNetwork, ferr.Kind)
	})
}

func serve(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		// A nil value stops net/http from sniffing a type of its own.
		w.Header()["Content-Type"] = nil
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		_, _ = w.Write(body)
	}
}

func TestTextTypesComeBackAsTheyAre(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/plain":        serve("text/plain; charset=utf-8", []byte("# not a heading\n<b>not bold</b>\n")),
		"/json":         serve("application/json", []byte(`{"name":"café","n":1}`)),
		"/latin1":       serve("text/plain; charset=iso-8859-1", []byte("caf\xe9 \xa35\n")),
		"/bom":          serve("text/plain", []byte("\xff\xfeh\x00i\x00")),
		"/guess":        serve("text/plain", []byte("caf\xe9\n")),
		"/untyped":      serve("", []byte("just some text with no type at all\n")),
		"/untyped-html": serve("", []byte("<!DOCTYPE html><html><body><h1>Sniffed</h1></body></html>")),
		"/user-agent": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(r.UserAgent()))
		},
	})
	cases := []struct {
		path, text, contentType, charset string
		sniffed, guessed                 bool
		format                           webfetch.Format
	}{
		// Markup in a text file is not converted.
		{"/plain", "# not a heading\n<b>not bold</b>\n", "text/plain", "utf-8", false, false, webfetch.FormatText},
		{"/json", `{"name":"café","n":1}`, "application/json", "utf-8", false, true, webfetch.FormatText},
		// The header's charset is honoured, so the bytes become UTF-8.
		{"/latin1", "café £5\n", "text/plain", "windows-1252", false, false, webfetch.FormatText},
		{"/bom", "hi", "text/plain", "utf-16le", false, false, webfetch.FormatText},
		// Nothing declares it and it is not UTF-8: read as windows-1252,
		// and the result says that was a guess.
		{"/guess", "café\n", "text/plain", "windows-1252", false, true, webfetch.FormatText},
		{"/untyped", "just some text with no type at all\n", "text/plain", "windows-1252", true, true, webfetch.FormatText},
		{"/untyped-html", "# Sniffed\n", "text/html", "windows-1252", true, true, webfetch.FormatMarkdown},
		{"/user-agent", "towa-test", "text/plain", "windows-1252", false, true, webfetch.FormatText},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			res, err := fetch(t, limits(), srv.URL+c.path)
			require.NoError(t, err)
			require.Equal(t, c.text, res.Text)
			require.Equal(t, c.contentType, res.ContentType)
			require.Equal(t, c.sniffed, res.Sniffed)
			require.Equal(t, c.charset, res.Charset)
			require.Equal(t, c.guessed, res.CharsetGuessed)
			require.Equal(t, c.format, res.Format)
		})
	}
}

func TestWhatCannotBeReturnedIsATypedError(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	srv := site(t, map[string]http.HandlerFunc{
		"/png":         serve("image/png", png),
		"/untyped-png": serve("", png),
		"/deep": serve("text/html", []byte(
			strings.Repeat("<div>", 600)+"deep"+strings.Repeat("</div>", 600))),
	})
	cases := []struct {
		name, url string
		kind      webfetch.Kind
		check     func(*testing.T, *webfetch.Error)
	}{
		{"file scheme", "file:///etc/hostname", webfetch.KindScheme, nil},
		{"no scheme", "example.org/page", webfetch.KindScheme, nil},
		{"no host", "http:///page", webfetch.KindInvalidURL, nil},
		{"does not parse", "http://exa mple.org/", webfetch.KindInvalidURL, nil},
		{"not found", srv.URL + "/posts/missing.html", webfetch.KindStatus, func(t *testing.T, e *webfetch.Error) {
			require.Equal(t, http.StatusNotFound, e.Status)
		}},
		{"an image", srv.URL + "/png", webfetch.KindContentType, func(t *testing.T, e *webfetch.Error) {
			require.Equal(t, "image/png", e.ContentType)
		}},
		{"an image with no type", srv.URL + "/untyped-png", webfetch.KindContentType, func(t *testing.T, e *webfetch.Error) {
			require.Equal(t, "image/png", e.ContentType)
		}},
		{"nested too deep to parse", srv.URL + "/deep", webfetch.KindParse, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := fetch(t, limits(), c.url)
			var ferr *webfetch.Error
			require.ErrorAs(t, err, &ferr)
			require.Equal(t, c.kind, ferr.Kind)
			if c.check != nil {
				c.check(t, ferr)
			}
		})
	}

	t.Run("nothing listens", func(t *testing.T) {
		closed := httptest.NewServer(http.NotFoundHandler())
		url := closed.URL
		closed.Close()
		_, err := fetch(t, limits(), url)
		var ferr *webfetch.Error
		require.ErrorAs(t, err, &ferr)
		require.Equal(t, webfetch.KindNetwork, ferr.Kind)
	})
}

func TestABodyOverTheLimitIsAnErrorAndNotHalfAPage(t *testing.T) {
	body := []byte(strings.Repeat("0123456789", 10))
	srv := site(t, map[string]http.HandlerFunc{
		"/declared": serve("text/plain", body),
		// Flushing before the body makes the response chunked, so the
		// server declares no length.
		"/undeclared": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			w.(http.Flusher).Flush()
			_, _ = w.Write(body)
		},
	})
	cfg := limits()
	cfg.MaxBodyBytes = int64(len(body)) - 1

	_, err := fetch(t, cfg, srv.URL+"/declared")
	var ferr *webfetch.Error
	require.ErrorAs(t, err, &ferr)
	require.Equal(t, webfetch.Error{
		Kind: webfetch.KindTooLarge, URL: srv.URL + "/declared", Size: int64(len(body)), Limit: cfg.MaxBodyBytes,
	}, *ferr)

	_, err = fetch(t, cfg, srv.URL+"/undeclared")
	require.ErrorAs(t, err, &ferr)
	require.Equal(t, webfetch.Error{
		Kind: webfetch.KindTooLarge, URL: srv.URL + "/undeclared", Size: -1, Limit: cfg.MaxBodyBytes,
	}, *ferr)

	// A body of exactly the limit is read whole, declared or not.
	cfg.MaxBodyBytes = int64(len(body))
	for _, path := range []string{"/declared", "/undeclared"} {
		res, err := fetch(t, cfg, srv.URL+path)
		require.NoError(t, err)
		require.Equal(t, string(body), res.Text)
		require.Equal(t, int64(len(body)), res.BodyBytes)
	}
}

func TestAResultOverTheLimitSaysHowMuchWasLeftOut(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		"/lines": serve("text/plain; charset=utf-8", []byte(strings.Repeat("a line of text\n", 40))),
		// No line break to cut at, and every character is two bytes.
		"/accents": serve("text/plain; charset=utf-8", []byte(strings.Repeat("é", 300))),
	})
	full, err := fetch(t, limits(), srv.URL+"/posts/article.html")
	require.NoError(t, err)

	for _, c := range []struct {
		url   string
		limit int64
	}{
		{srv.URL + "/posts/article.html", 700},
		{srv.URL + "/lines", 100},
		{srv.URL + "/accents", 101},
	} {
		cfg := limits()
		whole, err := fetch(t, cfg, c.url)
		require.NoError(t, err)
		cfg.MaxResultBytes = c.limit
		res, err := fetch(t, cfg, c.url)
		require.NoError(t, err, c.url)
		require.LessOrEqual(t, int64(len(res.Text)), c.limit, c.url)
		require.Positive(t, res.OmittedBytes, c.url)
		// What is kept is the start, and kept plus omitted is everything.
		require.True(t, strings.HasPrefix(whole.Text, res.Text), c.url)
		require.Equal(t, int64(len(whole.Text)), int64(len(res.Text))+res.OmittedBytes, c.url)
		require.True(t, utf8.ValidString(res.Text), c.url)
	}

	// With a line break in reach, the cut is at the end of a line.
	cfg := limits()
	cfg.MaxResultBytes = 100
	res, err := fetch(t, cfg, srv.URL+"/lines")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(res.Text, "a line of text\n"))

	// At exactly the length nothing is left out.
	cfg.MaxResultBytes = int64(len(full.Text))
	res, err = fetch(t, cfg, srv.URL+"/posts/article.html")
	require.NoError(t, err)
	require.Equal(t, full.Text, res.Text)
	require.Zero(t, res.OmittedBytes)
}

func TestATimeoutIsATypedErrorAndACancelIsNot(t *testing.T) {
	srv := site(t, map[string]http.HandlerFunc{
		// Never answers.
		"/silent": func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() },
		// Sends its headers and the start of the body, then stops.
		"/stalls": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("the first part"))
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		},
	})
	cfg := limits()
	cfg.Timeout = 200 * time.Millisecond
	for _, path := range []string{"/silent", "/stalls"} {
		_, err := fetch(t, cfg, srv.URL+path)
		var ferr *webfetch.Error
		require.ErrorAs(t, err, &ferr, path)
		require.Equal(t, webfetch.KindTimeout, ferr.Kind, path)
		require.Equal(t, int64(cfg.Timeout), ferr.Limit, path)
	}

	// The caller giving up is not something the model caused.
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := webfetch.Fetch(ctx, httpclient.New(), limits(), webfetch.Input{URL: srv.URL + "/silent"})
	require.ErrorIs(t, err, context.Canceled)
	var ferr *webfetch.Error
	require.NotErrorAs(t, err, &ferr)
}
