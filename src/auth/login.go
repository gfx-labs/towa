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
	"time"
)

// LoginOptions is what one login needs. Every field is required, except
// that OpenBrowser is not used by a manual login.
type LoginOptions struct {
	Flow Flow
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
}

// RunLogin takes the user through one authorization and returns the tokens.
// It stores nothing.
func RunLogin(ctx context.Context, opts LoginOptions) (Tokens, error) {
	if opts.Manual {
		return manualLogin(ctx, opts)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(opts.Flow.CallbackPort))
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return Tokens{}, fmt.Errorf("the browser cannot be sent back to %s: %w (a manual login needs no local port)", addr, err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	login := opts.Flow.Begin(opts.Flow.LocalRedirectURI(port))

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

	if _, err := fmt.Fprintf(opts.Out, "Opening this page in a browser:\n\n%s\n\n", login.AuthorizeURL()); err != nil {
		return Tokens{}, errors.Join(err, stop())
	}
	if err := opts.OpenBrowser(ctx, login.AuthorizeURL()); err != nil {
		return Tokens{}, errors.Join(fmt.Errorf("no browser could be opened: %w (a manual login needs none)", err), stop())
	}

	timeout := time.NewTimer(opts.CallbackTimeout)
	defer timeout.Stop()
	var result callback
	select {
	case result = <-got:
	case <-timeout.C:
		result.err = fmt.Errorf("the browser did not come back within %s (a manual login waits for a pasted code)", opts.CallbackTimeout)
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
	redirect := opts.Flow.ManualRedirectURI
	prompt := "Approve the login, then paste the code the page shows:"
	if redirect == "" {
		redirect = opts.Flow.LocalRedirectURI(opts.Flow.CallbackPort)
		prompt = "Approve the login. The browser then fails to open an address that starts with " + redirect +
			"\nPaste that whole address:"
	}
	login := opts.Flow.Begin(redirect)
	if _, err := fmt.Fprintf(opts.Out, "Open this page in a browser on any device:\n\n%s\n\n%s\n", login.AuthorizeURL(), prompt); err != nil {
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
		if errors.Is(err, io.EOF) && text != "" {
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
