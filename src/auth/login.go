package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrNothingPasted says a manual login was given no code: an empty line, or
// an input that ended.
var ErrNothingPasted = errors.New("no code was pasted")

// LoginOptions is what one login needs. Every field is required, except
// that only a browser login uses OpenBrowser and CallbackTimeout, and only
// a manual login reads In.
type LoginOptions struct {
	Flow Flow
	// Device shows a code to type into the provider's page and waits for
	// its approval. It needs no browser on this host and reads nothing.
	Device bool
	// HTTPClient sends the token request. It comes from src/httpclient.
	HTTPClient *http.Client
	// Manual asks for the code to be pasted and starts no callback server.
	// It is the login for a host without a browser.
	Manual bool
	// OpenBrowser opens the authorize page.
	OpenBrowser func(ctx context.Context, url string) error
	// In is where a pasted code is read from, and Out where the user is
	// told what to do.
	In  io.Reader
	Out io.Writer
	// CallbackTimeout is how long to wait for the browser to come back.
	CallbackTimeout time.Duration
	// TokenTimeout bounds the token request.
	TokenTimeout time.Duration
	// Later records a browser or manual login before its page is shown, so
	// that a later run of "acph login <provider> --code" can finish
	// it. It may be nil, and then the user is not told of that way.
	Later func(login *Login) error
}

// later records login for a later run and returns what to tell the user
// about it. what names the thing the later run is given.
func (opts LoginOptions) later(login *Login, what string) (string, error) {
	if opts.Later == nil {
		return "", nil
	}
	if err := opts.Later(login); err != nil {
		return "", err
	}
	return fmt.Sprintf("A later run can also finish this login, given %s:\n\n  acph login %s --code '...'\n\n", what, opts.Flow.Provider), nil
}

// RunLogin takes the user through one authorization and returns the tokens.
// It stores nothing.
func RunLogin(ctx context.Context, opts LoginOptions) (Tokens, error) {
	switch {
	case opts.Device:
		return deviceLogin(ctx, opts)
	case opts.Manual:
		return manualLogin(ctx, opts)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Flow.CallbackPort))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return Tokens{}, fmt.Errorf("the browser cannot be sent back to %s: %w (a manual login needs no local port)", addr, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	login := opts.Flow.Begin(opts.Flow.LocalRedirectURI(port))
	hint, err := opts.later(login, "the whole address the browser ends on")
	if err != nil {
		return Tokens{}, errors.Join(err, listener.Close())
	}

	type callback struct {
		code string
		err  error
	}
	got := make(chan callback, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+opts.Flow.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var result callback
		switch {
		case q.Get("state") != login.State():
			// Not an answer to this login: some other page or an old tab.
			http.Error(w, "This is not the login acpharness is waiting for.", http.StatusBadRequest)
			return
		case q.Get("error") != "":
			result.err = fmt.Errorf("%s refused the login: %s %s", opts.Flow.Provider, q.Get("error"), q.Get("error_description"))
			http.Error(w, "The login was refused. You can close this tab.", http.StatusBadRequest)
		case q.Get("code") == "":
			http.Error(w, "This request carries no authorization code.", http.StatusBadRequest)
			return
		default:
			result.code = q.Get("code")
			_, _ = io.WriteString(w, "acpharness has the authorization code. You can close this tab.\n")
		}
		select {
		case got <- result:
		default: // a second answer to the same login changes nothing
		}
	})
	server := &http.Server{Handler: mux}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	stop := func() error {
		// The answer page is written before the code is handed over, so
		// nothing is cut off by closing at once.
		err := server.Close()
		if serveErr := <-served; !errors.Is(serveErr, http.ErrServerClosed) {
			err = errors.Join(err, serveErr)
		}
		return err
	}

	if _, err := fmt.Fprintf(opts.Out, "Open this page in a browser on this host to log in:\n\n%s\n\n%s", login.AuthorizeURL(), hint); err != nil {
		return Tokens{}, errors.Join(err, stop())
	}
	// The page is printed, so a host without an opener is not the end of
	// the login: the user opens it.
	if err := opts.OpenBrowser(ctx, login.AuthorizeURL()); err != nil {
		if _, err := fmt.Fprintf(opts.Out, "No browser could be opened from here (%v). Open the page yourself.\n\n", err); err != nil {
			return Tokens{}, errors.Join(err, stop())
		}
	}

	timeout := time.NewTimer(opts.CallbackTimeout)
	defer timeout.Stop()
	var result callback
	select {
	case result = <-got:
	case <-timeout.C:
		result.err = fmt.Errorf("the browser did not come back within %s", opts.CallbackTimeout)
		if hint != "" {
			result.err = fmt.Errorf("%w: the login can still be finished with \"acph login %s --code '<the address the browser ended on>'\"", result.err, opts.Flow.Provider)
		}
	case <-ctx.Done():
		result.err = context.Cause(ctx)
	}
	if err := errors.Join(result.err, stop()); err != nil {
		return Tokens{}, err
	}
	return exchange(ctx, opts, login, result.code, login.State())
}

// manualLogin prints the page to open, on any device, and reads what the
// user copies from the page it ends on.
func manualLogin(ctx context.Context, opts LoginOptions) (Tokens, error) {
	login, what := opts.Flow.BeginManual()
	hint, err := opts.later(login, what)
	if err != nil {
		return Tokens{}, err
	}
	if _, err := fmt.Fprintf(opts.Out, "Open this page in a browser on any device:\n\n%s\n\nApprove the login, then paste %s:\n\n%s", login.AuthorizeURL(), what, hint); err != nil {
		return Tokens{}, err
	}
	// The line is read in a goroutine so that a cancelled context ends the
	// login. The goroutine ends with the process or the next line.
	type line struct {
		text string
		err  error
	}
	read := make(chan line, 1)
	go func() {
		text, err := bufio.NewReader(opts.In).ReadString('\n')
		if errors.Is(err, io.EOF) {
			err = nil
		}
		read <- line{text, err}
	}()
	var pasted line
	select {
	case pasted = <-read:
	case <-ctx.Done():
		return Tokens{}, context.Cause(ctx)
	}
	if pasted.err != nil {
		return Tokens{}, fmt.Errorf("reading the pasted code: %w", pasted.err)
	}
	// An empty line or a closed input: nobody pasted here.
	if strings.TrimSpace(pasted.text) == "" {
		return Tokens{}, ErrNothingPasted
	}
	code, state, err := ParsePasted(pasted.text)
	if err != nil {
		return Tokens{}, err
	}
	return exchange(ctx, opts, login, code, state)
}

func exchange(ctx context.Context, opts LoginOptions, login *Login, code, state string) (Tokens, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.TokenTimeout)
	defer cancel()
	return login.Exchange(ctx, opts.HTTPClient, code, state)
}
