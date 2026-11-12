// Package webfetch fetches one URL and returns its content as text a model
// can read: Markdown for an HTML page, the text itself for a text type. It
// is one capability with typed input and a typed result. The tool that calls
// it turns a result or an *Error into the text the model reads.
package webfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/gfx-labs/towa/src/config"
)

// Kind says why a fetch was refused or failed in a way the model can act
// on.
type Kind string

// Kinds of an Error.
const (
	// KindInvalidURL: the URL does not parse or has no host.
	KindInvalidURL Kind = "invalid_url"
	// KindScheme: the URL is not http or https.
	KindScheme Kind = "scheme"
	// KindTimeout: the fetch took longer than webfetch.timeout. Limit holds
	// the timeout in nanoseconds.
	KindTimeout Kind = "timeout"
	// KindNetwork: no response arrived, or the body was cut off. Err says
	// why: a name that does not resolve, a refused connection, a
	// certificate, too many redirects.
	KindNetwork Kind = "network"
	// KindStatus: the server answered with a status that is not 2xx. Status
	// holds it.
	KindStatus Kind = "status"
	// KindTooLarge: the body is over webfetch.max_body_bytes. Limit holds
	// the limit, and Size the length the server declared, or -1 when it
	// declared none.
	KindTooLarge Kind = "too_large"
	// KindContentType: the content is neither HTML nor text. ContentType
	// holds what it is.
	KindContentType Kind = "content_type"
	// KindParse: the HTML could not be parsed. The parser refuses a
	// document nested deeper than 512 elements.
	KindParse Kind = "parse"
)

// Error is a failure the model caused or can act on. Any other error from
// this package means the harness could not do the fetch, or the call was
// cancelled.
type Error struct {
	Kind Kind
	// URL is the URL that failed: the final one once a response arrived,
	// and the one that was asked for before that.
	URL string
	// Status is the HTTP status, for KindStatus.
	Status int
	// ContentType is the media type, for KindContentType.
	ContentType string
	// Size and Limit are in bytes for KindTooLarge. For KindTimeout, Limit
	// is the timeout in nanoseconds.
	Size  int64
	Limit int64
	Err   error
}

func (e *Error) Error() string {
	s := fmt.Sprintf("webfetch: %s: %s", e.Kind, e.URL)
	switch e.Kind {
	case KindStatus:
		s += fmt.Sprintf(": HTTP %d", e.Status)
	case KindContentType:
		s += ": " + e.ContentType
	case KindTooLarge:
		s += fmt.Sprintf(": over %d bytes", e.Limit)
	}
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}

func (e *Error) Unwrap() error { return e.Err }

// Format is the form of a result's text.
type Format string

// Formats of a result.
const (
	// FormatMarkdown: the page was HTML and Text is its Markdown.
	FormatMarkdown Format = "markdown"
	// FormatText: Text is the body as it was served, decoded to UTF-8.
	FormatText Format = "text"
)

// Input names the URL to fetch.
type Input struct {
	URL string
}

// Result is the content of a fetched URL.
type Result struct {
	// URL is the URL the content came from, after redirects.
	URL string
	// Status is the HTTP status, which is 2xx.
	Status int
	// ContentType is the media type the content was treated as, without
	// parameters. Sniffed says the server sent none and it was worked out
	// from the first bytes, as a browser does.
	ContentType string
	Sniffed     bool
	// Charset is the encoding the body was decoded from. CharsetGuessed
	// says nothing declared it: no header parameter, no byte order mark and
	// no meta element.
	Charset        string
	CharsetGuessed bool
	Format         Format
	// Title is the page's title element, for FormatMarkdown.
	Title string
	// Text is the content. It is at most webfetch.max_result_bytes long.
	Text string
	// BodyBytes is how many bytes the server sent, after decompression.
	BodyBytes int64
	// OmittedBytes is how many bytes of the converted text were left out of
	// Text because it was over webfetch.max_result_bytes. They are the end
	// of the text.
	OmittedBytes int64
}

// errTimeout is the cause set on the context when webfetch.timeout passes,
// which tells it apart from the caller's own deadline.
var errTimeout = errors.New("webfetch: timeout")

// Fetch gets one http or https URL with client and returns its content.
// The client follows redirects. A cancelled ctx returns ctx's error, not an
// *Error.
func Fetch(ctx context.Context, client *http.Client, cfg config.WebFetch, in Input) (Result, error) {
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil {
		return Result{}, &Error{Kind: KindInvalidURL, URL: in.URL, Err: err}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Result{}, &Error{Kind: KindScheme, URL: in.URL}
	}
	if u.Hostname() == "" {
		return Result{}, &Error{Kind: KindInvalidURL, URL: in.URL}
	}

	fetchCtx, cancel := context.WithTimeoutCause(ctx, cfg.Timeout, errTimeout)
	defer cancel()
	// transportError sorts a failed request or read: the caller's
	// cancellation, our timeout, or the network.
	transportError := func(at string, err error) error {
		switch {
		case ctx.Err() != nil:
			return fmt.Errorf("webfetch: %w", context.Cause(ctx))
		case errors.Is(context.Cause(fetchCtx), errTimeout):
			return &Error{Kind: KindTimeout, URL: at, Limit: int64(cfg.Timeout)}
		default:
			return &Error{Kind: KindNetwork, URL: at, Err: err}
		}
	}

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Result{}, &Error{Kind: KindInvalidURL, URL: in.URL, Err: err}
	}
	req.Header.Set("User-Agent", cfg.UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, transportError(in.URL, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing was written
	final := resp.Request.URL
	at := final.String()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, &Error{Kind: KindStatus, URL: at, Status: resp.StatusCode}
	}
	// Checked before the type, so that a download is refused for its size
	// without a byte of it being read.
	if resp.ContentLength > cfg.MaxBodyBytes {
		return Result{}, &Error{Kind: KindTooLarge, URL: at, Size: resp.ContentLength, Limit: cfg.MaxBodyBytes}
	}
	header := resp.Header.Get("Content-Type")
	mediaType := ""
	if header != "" {
		// A malformed header is treated as none: the type is then sniffed.
		if parsed, _, err := mime.ParseMediaType(header); err == nil {
			mediaType = parsed
		}
	}
	if mediaType != "" && formatOf(mediaType) == "" {
		return Result{}, &Error{Kind: KindContentType, URL: at, ContentType: mediaType}
	}

	// One byte over the limit is read, to tell a body at the limit from a
	// longer one whose length the server did not declare.
	body, err := io.ReadAll(io.LimitReader(resp.Body, cfg.MaxBodyBytes+1))
	if err != nil {
		return Result{}, transportError(at, err)
	}
	if int64(len(body)) > cfg.MaxBodyBytes {
		return Result{}, &Error{Kind: KindTooLarge, URL: at, Size: resp.ContentLength, Limit: cfg.MaxBodyBytes}
	}

	res := Result{URL: at, Status: resp.StatusCode, ContentType: mediaType, BodyBytes: int64(len(body))}
	if mediaType == "" {
		// The algorithm browsers use for a response without a type.
		sniffed, _, err := mime.ParseMediaType(http.DetectContentType(body))
		if err != nil {
			return Result{}, fmt.Errorf("webfetch: sniffed content type: %w", err)
		}
		res.ContentType, res.Sniffed = sniffed, true
		if formatOf(sniffed) == "" {
			return Result{}, &Error{Kind: KindContentType, URL: at, ContentType: sniffed}
		}
	}
	res.Format = formatOf(res.ContentType)

	// The byte order mark, then the header's charset, then a meta element,
	// then UTF-8 if the bytes are valid UTF-8, then windows-1252: the order
	// a browser uses. Only the last two are guesses.
	enc, name, certain := charset.DetermineEncoding(body[:min(len(body), 1024)], header)
	res.Charset, res.CharsetGuessed = name, !certain
	decoded, err := enc.NewDecoder().Bytes(body)
	if err != nil {
		return Result{}, &Error{Kind: KindNetwork, URL: at, Err: fmt.Errorf("decoding %s: %w", name, err)}
	}
	// A byte that is not valid in the encoding becomes U+FFFD, as a browser
	// shows it. The decoders of the other encodings already do this.
	text := strings.ToValidUTF8(string(decoded), string(utf8.RuneError))
	// The byte order mark named the encoding. It is not part of the text.
	text = strings.TrimPrefix(text, "\ufeff")

	if res.Format == FormatMarkdown {
		md, title, err := toMarkdown(text, final)
		if err != nil {
			return Result{}, &Error{Kind: KindParse, URL: at, Err: err}
		}
		text, res.Title = md, title
	}
	res.Text, res.OmittedBytes = cut(text, cfg.MaxResultBytes)
	return res, nil
}

// formatOf returns how a media type is returned, or "" when it is not one
// this package returns.
func formatOf(mediaType string) Format {
	switch {
	case mediaType == "text/html", mediaType == "application/xhtml+xml":
		return FormatMarkdown
	case strings.HasPrefix(mediaType, "text/"),
		mediaType == "application/json", mediaType == "application/xml",
		mediaType == "application/javascript", mediaType == "application/x-ndjson",
		strings.HasSuffix(mediaType, "+json"), strings.HasSuffix(mediaType, "+xml"):
		return FormatText
	default:
		return ""
	}
}

// cut returns at most limit bytes of text and how many bytes it left out.
// It cuts between characters, and at the end of a line when there is one in
// the second half of what is kept.
func cut(text string, limit int64) (string, int64) {
	if int64(len(text)) <= limit {
		return text, 0
	}
	end := int(limit)
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	if nl := strings.LastIndexByte(text[:end], '\n'); nl >= end/2 {
		end = nl + 1
	}
	return text[:end], int64(len(text) - end)
}
