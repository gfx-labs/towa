package auth_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/support/httpclient"
)

// The variables that make the test binary act as one towa process
// asking for a credential.
const (
	envChildDir = "TOWA_AUTH_TEST_DIR"
	envChildURL = "TOWA_AUTH_TEST_TOKEN_URL"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(envChildDir); dir != "" {
		src, err := auth.NewSource(sourceOptions(dir, os.Getenv(envChildURL), true))
		if err == nil {
			var cred auth.Credential
			if cred, err = src.Credential(context.Background(), "work"); err == nil {
				fmt.Print(cred.Token)
				os.Exit(0)
			}
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func sourceOptions(dir, tokenURL string, jsonRequests bool) auth.Options {
	flow := auth.Flow{Provider: "claude", ClientID: "client", TokenURL: tokenURL, JSONTokenRequests: jsonRequests}
	return auth.Options{
		Accounts: []config.Account{
			{ID: "work", Provider: "claude", Kind: config.AccountOAuth},
			{ID: "key", Provider: "openrouter", Kind: config.AccountAPIKey, APIKeyEnv: "TEST_KEY"},
			{ID: "held", Provider: "codex", Kind: config.AccountProxiedOAuth, ChatGPTAccountID: "acct-1"},
			{ID: "heldkey", Provider: "openrouter", Kind: config.AccountProxiedAPIKey},
			{ID: "open", Provider: "lan", Kind: config.AccountNone},
		},
		Store:      auth.NewStore(dir),
		HTTPClient: httpclient.New(),
		Getenv: func(name string) string {
			if name == "TEST_KEY" {
				return "sk-from-env"
			}
			return ""
		},
		Flows:         map[string]auth.Flow{"claude": flow},
		RefreshMargin: time.Minute,
		TokenTimeout:  time.Minute,
	}
}

// tokenEndpoint is a token endpoint that rotates the refresh token, as the
// real ones do: a refresh token works once.
type tokenEndpoint struct {
	*httptest.Server
	mu       sync.Mutex
	requests int
	refused  int
	current  string // the refresh token that works now
	grants   []map[string]string
	idToken  string
	// polls counts the requests for a device code's approval.
	polls int
	// challenge is what the authorize page was given.
	challenge string
}

func newTokenEndpoint(t *testing.T, refreshToken string) *tokenEndpoint {
	e := &tokenEndpoint{current: refreshToken}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		grant := map[string]string{}
		if r.Header.Get("Content-Type") == "application/json" {
			if err := json.UnmarshalRead(r.Body, &grant); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
		} else {
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			for k := range r.PostForm {
				grant[k] = r.PostForm.Get(k)
			}
		}
		// A slow endpoint, so that every waiting process is at the lock
		// before the first refresh ends.
		time.Sleep(200 * time.Millisecond)

		e.mu.Lock()
		defer e.mu.Unlock()
		e.requests++
		e.grants = append(e.grants, grant)
		n := e.requests
		switch grant["grant_type"] {
		case "refresh_token":
			if grant["refresh_token"] != e.current {
				e.refused++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token already used"}`)
				return
			}
		case "authorization_code":
			sum := sha256.Sum256([]byte(grant["code_verifier"]))
			if grant["code"] != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != e.challenge {
				e.refused++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
				return
			}
		default:
			http.Error(w, "unsupported grant", http.StatusBadRequest)
			return
		}
		e.current = fmt.Sprintf("refresh-%d", n)
		_ = json.MarshalWrite(w, map[string]any{
			"access_token":  fmt.Sprintf("access-%d", n),
			"refresh_token": e.current,
			"expires_in":    3600,
			"scope":         "user:inference user:profile",
			"id_token":      e.idToken,
		})
	})
	// The authorize page of a provider whose user approves at once: it
	// sends the browser back with a code.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		e.mu.Lock()
		e.challenge = q.Get("code_challenge")
		e.mu.Unlock()
		back := q.Get("redirect_uri") + "?" + url.Values{"code": {"the-code"}, "state": {q.Get("state")}}.Encode()
		http.Redirect(w, r, back, http.StatusFound)
	})
	// The device code endpoints of a provider whose user approves between
	// the first request for the approval and the second.
	mux.HandleFunc("POST /deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"1"}`)
	})
	mux.HandleFunc("POST /deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		poll := map[string]string{}
		if err := json.UnmarshalRead(r.Body, &poll); err != nil || poll["device_auth_id"] != "dev-1" || poll["user_code"] != "ABCD-1234" {
			http.Error(w, "bad poll", http.StatusBadRequest)
			return
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.polls++
		if e.polls == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// The provider chooses the verifier of a device login.
		sum := sha256.Sum256([]byte("device-verifier"))
		e.challenge = base64.RawURLEncoding.EncodeToString(sum[:])
		_ = json.MarshalWrite(w, map[string]string{
			"authorization_code": "the-code", "code_challenge": e.challenge, "code_verifier": "device-verifier",
		})
	})
	e.Server = httptest.NewServer(mux)
	t.Cleanup(e.Close)
	return e
}

func (e *tokenEndpoint) counts() (requests, refused int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.requests, e.refused
}

func expired(refreshToken string) auth.Tokens {
	return auth.Tokens{
		Provider: "claude", AccessToken: "access-old", RefreshToken: refreshToken,
		ExpiresAtUnixMs: time.Now().Add(-time.Hour).UnixMilli(),
	}
}

// Many processes and goroutines find the same expired token at once. One of
// them refreshes. The endpoint refuses a refresh token used twice, so a
// second refresh would also fail a process.
func TestProcessesShareOneRefresh(t *testing.T) {
	endpoint := newTokenEndpoint(t, "refresh-0")
	dir := filepath.Join(t.TempDir(), "state", "auth")
	require.NoError(t, auth.NewStore(dir).Save("work", expired("refresh-0")))

	const processes, goroutines = 12, 12
	tokens := make(chan string, processes+goroutines)
	var wg sync.WaitGroup
	for range processes {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), envChildDir+"="+dir, envChildURL+"="+endpoint.URL+"/token")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				tokens <- "process failed: " + stderr.String()
				return
			}
			tokens <- string(out)
		})
	}
	src, err := auth.NewSource(sourceOptions(dir, endpoint.URL+"/token", true))
	require.NoError(t, err)
	for range goroutines {
		wg.Go(func() {
			cred, err := src.Credential(context.Background(), "work")
			if err != nil {
				tokens <- "goroutine failed: " + err.Error()
				return
			}
			tokens <- cred.Token
		})
	}
	wg.Wait()
	close(tokens)
	for token := range tokens {
		require.Equal(t, "access-1", token)
	}
	requests, refused := endpoint.counts()
	require.Equal(t, 1, requests)
	require.Zero(t, refused)

	// What is left on disk: the one file of logins and the lock files,
	// readable by the owner only, and no temporary file.
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := []string{}
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), entry.Name())
		names = append(names, entry.Name())
	}
	require.Equal(t, []string{"account.work.lock", "auth.json", "auth.json.lock"}, names)
	stored, err := auth.NewStore(dir).Load("work")
	require.NoError(t, err)
	require.Equal(t, "refresh-1", stored.RefreshToken)
	require.Equal(t, []string{"user:inference", "user:profile"}, stored.Scopes)
}

// A provider refused a token whose expiry is still ahead. The first caller
// refreshes, and a second caller holding the same refused token gets the
// new one without another request.
func TestRefreshAfterRejection(t *testing.T) {
	endpoint := newTokenEndpoint(t, "refresh-0")
	dir := t.TempDir()
	login := expired("refresh-0")
	login.ExpiresAtUnixMs = time.Now().Add(time.Hour).UnixMilli()
	require.NoError(t, auth.NewStore(dir).Save("work", login))
	// A form endpoint this time, as Codex's is.
	src, err := auth.NewSource(sourceOptions(dir, endpoint.URL+"/token", false))
	require.NoError(t, err)

	rejected, err := src.Credential(t.Context(), "work")
	require.NoError(t, err)
	require.Equal(t, "access-old", rejected.Token)
	for range 2 {
		cred, err := src.Refresh(t.Context(), rejected)
		require.NoError(t, err)
		require.Equal(t, auth.Credential{Account: "work", Scheme: auth.SchemeOAuth, Token: "access-1"}, cred)
	}
	requests, _ := endpoint.counts()
	require.Equal(t, 1, requests)
	require.Equal(t, "client", endpoint.grants[0]["client_id"])

	// The endpoint now refuses: the error carries its answer, and the
	// stored login is left as it was.
	endpoint.mu.Lock()
	endpoint.current = "revoked"
	endpoint.mu.Unlock()
	_, err = src.Refresh(t.Context(), auth.Credential{Account: "work", Token: "access-1"})
	var refusal *auth.TokenError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, http.StatusBadRequest, refusal.Status)
	require.JSONEq(t, `"invalid_grant"`, string(refusal.Detail))
	var relogin *auth.LoginRequiredError
	require.ErrorAs(t, err, &relogin)
	require.Equal(t, "invalid_grant", relogin.Code)
	stored, err := auth.NewStore(dir).Load("work")
	require.NoError(t, err)
	require.Equal(t, "access-1", stored.AccessToken)

	// The refresh token is recorded as dead: neither call sends anything
	// now, in this process or a new one, and both say to log in again.
	fresh, err := auth.NewSource(sourceOptions(dir, endpoint.URL+"/token", false))
	require.NoError(t, err)
	_, err = fresh.Credential(t.Context(), "work")
	require.ErrorAs(t, err, &relogin)
	require.WithinDuration(t, time.Now(), relogin.RejectedAt, time.Minute)
	_, err = src.Refresh(t.Context(), auth.Credential{Account: "work", Token: "access-1"})
	require.ErrorAs(t, err, &relogin)
	requests, _ = endpoint.counts()
	require.Equal(t, 2, requests)

	// A new login replaces the file, and the account works again.
	require.NoError(t, auth.NewStore(dir).Save("work", login))
	cred, err := src.Credential(t.Context(), "work")
	require.NoError(t, err)
	require.Equal(t, "access-old", cred.Token)
}

// An endpoint that is down says nothing about the refresh token, so the
// next call tries again.
func TestRefreshIsTriedAgainAfterATransientFailure(t *testing.T) {
	var requests atomic.Int32
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)
	dir := t.TempDir()
	require.NoError(t, auth.NewStore(dir).Save("work", expired("refresh-0")))
	src, err := auth.NewSource(sourceOptions(dir, down.URL, true))
	require.NoError(t, err)
	for range 2 {
		_, err = src.Credential(t.Context(), "work")
		var refusal *auth.TokenError
		require.ErrorAs(t, err, &refusal)
		var relogin *auth.LoginRequiredError
		require.NotErrorAs(t, err, &relogin)
	}
	require.Equal(t, int32(2), requests.Load())
}

// MCP logins: a server named by a client may have any name, the files are
// as private as an account's, and the lock excludes a second holder.
func TestMCPTokenStorage(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	store := auth.NewStore(dir)
	names := []string{"linear", "Linear", "../../etc/passwd", ".hidden", "a b/c"}
	for i, name := range names {
		_, err := store.LoadMCP(name)
		require.ErrorIs(t, err, auth.ErrNotLoggedIn, name)
		require.NoError(t, store.SaveMCP(name, auth.MCPTokens{ServerURL: "https://example.test/mcp", AccessToken: fmt.Sprint("access-", i)}))
	}
	for i, name := range names {
		got, err := store.LoadMCP(name)
		require.NoError(t, err, name)
		require.Equal(t, fmt.Sprint("access-", i), got.AccessToken, name)
	}
	require.Error(t, store.SaveMCP("", auth.MCPTokens{}))

	// Every file is in the one directory, whatever the name held.
	info, err := os.Stat(filepath.Join(dir, "mcp"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	entries, err := os.ReadDir(filepath.Join(dir, "mcp"))
	require.NoError(t, err)
	require.Len(t, entries, len(names))
	for _, entry := range entries {
		info, err := entry.Info()
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	top, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, top, 1)

	unlock, err := store.LockMCP(t.Context(), "linear")
	require.NoError(t, err)
	waiting, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = store.LockMCP(waiting, "linear")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	other, err := store.LockMCP(t.Context(), "Linear")
	require.NoError(t, err)
	require.NoError(t, other())
	require.NoError(t, unlock())
	again, err := store.LockMCP(t.Context(), "linear")
	require.NoError(t, err)
	require.NoError(t, again())

	require.NoError(t, store.DeleteMCP("linear"))
	require.ErrorIs(t, store.DeleteMCP("linear"), auth.ErrNotLoggedIn)
	_, err = store.LoadMCP("Linear")
	require.NoError(t, err)
}

func TestAccountsWithoutAStoredLogin(t *testing.T) {
	dir := t.TempDir()
	src, err := auth.NewSource(sourceOptions(dir, "http://127.0.0.1:1/unused", true))
	require.NoError(t, err)
	ctx := t.Context()

	key, err := src.Credential(ctx, "key")
	require.NoError(t, err)
	require.Equal(t, auth.Credential{Account: "key", Scheme: auth.SchemeAPIKey, Token: "sk-from-env"}, key)
	_, err = src.Refresh(ctx, key)
	require.ErrorIs(t, err, auth.ErrStaticKey)

	open, err := src.Credential(ctx, "open")
	require.NoError(t, err)
	require.Equal(t, auth.Credential{Account: "open", Scheme: auth.SchemeNone}, open)
	_, err = src.Refresh(ctx, open)
	require.ErrorIs(t, err, auth.ErrNoCredential)

	held, err := src.Credential(ctx, "held")
	require.NoError(t, err)
	require.True(t, held.Proxied)
	require.Equal(t, auth.SchemeOAuth, held.Scheme)
	require.Contains(t, held.Token, "held")
	require.Equal(t, "acct-1", held.ChatGPTAccountID)
	_, err = src.Refresh(ctx, held)
	require.ErrorIs(t, err, auth.ErrProxied)

	heldKey, err := src.Credential(ctx, "heldkey")
	require.NoError(t, err)
	require.True(t, heldKey.Proxied)
	require.Equal(t, auth.SchemeAPIKey, heldKey.Scheme)
	_, err = src.Refresh(ctx, heldKey)
	require.ErrorIs(t, err, auth.ErrProxied)

	_, err = src.Credential(ctx, "nobody")
	require.ErrorIs(t, err, auth.ErrUnknownAccount)
	_, err = src.Refresh(ctx, auth.Credential{Account: "nobody"})
	require.ErrorIs(t, err, auth.ErrUnknownAccount)
	_, err = src.Credential(ctx, "work")
	require.ErrorIs(t, err, auth.ErrNotLoggedIn)

	// None of it made a file.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestStoreRefusesAFileOthersCanRead(t *testing.T) {
	dir := t.TempDir()
	store := auth.NewStore(dir)
	require.NoError(t, store.Save("work", expired("r")))
	// Every account is in the one file, and the store names them.
	other := expired("r2")
	other.Provider = "codex"
	require.NoError(t, store.Save("second", other))
	accounts, err := store.Accounts()
	require.NoError(t, err)
	require.Equal(t, map[string]string{"work": "claude", "second": "codex"}, accounts)
	require.NoError(t, store.Delete("second"))
	kept, err := store.Load("work")
	require.NoError(t, err)
	require.Equal(t, "r", kept.RefreshToken)

	require.NoError(t, os.Chmod(filepath.Join(dir, "auth.json"), 0o644))
	_, err = store.Load("work")
	require.Error(t, err)
	require.NotErrorIs(t, err, auth.ErrNotLoggedIn)

	require.Error(t, store.Save("../escape", expired("r")))
}

// browser follows the authorize page's redirect to the local callback, as
// a browser whose user approved would.
func browser(ctx context.Context, page string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
	if err != nil {
		return err
	}
	resp, err := httpclient.New().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close() //nolint:errcheck // test
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("callback answered %d", resp.StatusCode)
	}
	return nil
}

func loginOptions(e *tokenEndpoint, flow auth.Flow) auth.LoginOptions {
	flow.AuthorizeURL = e.URL + "/authorize"
	flow.TokenURL = e.URL + "/token"
	flow.CallbackPort = 0
	return auth.LoginOptions{
		Flow: flow, HTTPClient: httpclient.New(), OpenBrowser: browser,
		In: strings.NewReader(""), Out: io.Discard,
		CallbackTimeout: time.Minute, TokenTimeout: time.Minute,
	}
}

// The Codex login through the browser: the callback's code is exchanged
// with the verifier of the challenge the authorize page saw, as a form, and
// the ChatGPT account comes out of the id token.
func TestLoginThroughTheBrowserCallback(t *testing.T) {
	endpoint := newTokenEndpoint(t, "")
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-42"}}`))
	endpoint.idToken = "e30." + claims + ".sig"

	tokens, err := auth.RunLogin(t.Context(), loginOptions(endpoint, auth.CodexFlow()))
	require.NoError(t, err)
	require.Equal(t, "access-1", tokens.AccessToken)
	require.Equal(t, "refresh-1", tokens.RefreshToken)
	require.Equal(t, "acct-42", tokens.ChatGPTAccountID)
	require.Equal(t, "codex", tokens.Provider)
	require.WithinDuration(t, time.Now().Add(time.Hour), tokens.ExpiresAt(), time.Minute)

	grant := endpoint.grants[0]
	require.Regexp(t, `^http://localhost:\d+/auth/callback$`, grant["redirect_uri"])
	require.NotContains(t, grant, "state")
}

// The Claude login on a headless host: the user pastes "code#state" from
// the provider's page, and the exchange is JSON with the state in it.
func TestLoginWithAPastedCode(t *testing.T) {
	endpoint := newTokenEndpoint(t, "")
	opts := loginOptions(endpoint, auth.ClaudeFlow())
	opts.Manual = true
	opts.OpenBrowser = nil
	printed, pasted := io.Pipe()
	pasteReader, pasteWriter := io.Pipe()
	opts.Out, opts.In = pasted, pasteReader

	// The user: reads the page address, opens it elsewhere, and pastes
	// what the provider's page shows.
	go func() {
		buf := make([]byte, 4096)
		var text string
		for !strings.Contains(text, "paste") {
			n, err := printed.Read(buf)
			if err != nil {
				return
			}
			text += string(buf[:n])
		}
		go func() { _, _ = io.Copy(io.Discard, printed) }()
		_, page, _ := strings.Cut(text, "\n\n")
		page, _, _ = strings.Cut(page, "\n")
		client := httpclient.New()
		client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := client.Get(page)
		if err != nil {
			_ = pasteWriter.CloseWithError(err)
			return
		}
		_ = resp.Body.Close()
		back, err := url.Parse(resp.Header.Get("Location"))
		if err != nil {
			_ = pasteWriter.CloseWithError(err)
			return
		}
		_, _ = fmt.Fprintf(pasteWriter, "  %s#%s\n", back.Query().Get("code"), back.Query().Get("state"))
	}()

	tokens, err := auth.RunLogin(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, "access-1", tokens.AccessToken)
	require.Equal(t, "claude", tokens.Provider)

	grant := endpoint.grants[0]
	require.Equal(t, auth.ClaudeFlow().ManualRedirectURI, grant["redirect_uri"])
	require.NotEmpty(t, grant["state"])
}

// The Codex login by device code: the code and its page are shown, the
// approval is asked for until it comes, and the code it carries is
// exchanged with the verifier the provider chose.
func TestLoginWithADeviceCode(t *testing.T) {
	endpoint := newTokenEndpoint(t, "")
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-42"}}`))
	endpoint.idToken = "e30." + claims + ".sig"
	opts := loginOptions(endpoint, auth.CodexFlow())
	opts.Device = true
	opts.OpenBrowser = nil
	opts.Flow.Device.UserCodeURL = endpoint.URL + "/deviceauth/usercode"
	opts.Flow.Device.PollURL = endpoint.URL + "/deviceauth/token"
	var shown bytes.Buffer
	opts.Out = &shown

	tokens, err := auth.RunLogin(t.Context(), opts)
	require.NoError(t, err)
	require.Equal(t, "access-1", tokens.AccessToken)
	require.Equal(t, "acct-42", tokens.ChatGPTAccountID)
	require.Contains(t, shown.String(), "ABCD-1234")
	require.Contains(t, shown.String(), auth.CodexFlow().Device.VerificationURL)
	require.Equal(t, 2, endpoint.polls)
	require.Equal(t, auth.CodexFlow().Device.RedirectURI, endpoint.grants[0]["redirect_uri"])

	// A provider without a device login says so and sends nothing.
	opts.Flow = auth.ClaudeFlow()
	_, err = auth.RunLogin(t.Context(), opts)
	require.Error(t, err)
	requests, _ := endpoint.counts()
	require.Equal(t, 1, requests)
}

// A code pasted with another login's state is not sent to the endpoint.
func TestLoginRefusesAnotherLoginsState(t *testing.T) {
	endpoint := newTokenEndpoint(t, "")
	opts := loginOptions(endpoint, auth.ClaudeFlow())
	opts.Manual = true
	opts.In = strings.NewReader("https://platform.claude.com/oauth/code/callback?code=the-code&state=someone-elses\n")
	_, err := auth.RunLogin(t.Context(), opts)
	require.Error(t, err)
	requests, _ := endpoint.counts()
	require.Zero(t, requests)
}
