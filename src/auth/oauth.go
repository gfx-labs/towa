package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Flow is one provider's OAuth setup: what a login and a refresh send, and
// where. The values of the built-in flows are those jcode uses
// (crates/jcode-base/src/auth/oauth.rs), which are the upstream CLIs' own.
type Flow struct {
	// Provider is the account provider the flow logs in to.
	Provider     string
	ClientID     string
	AuthorizeURL string
	TokenURL     string
	// Scopes is asked for at login and RefreshScopes at refresh. An empty
	// RefreshScopes sends no scope.
	Scopes        string
	RefreshScopes string
	// AuthorizeParams are the provider's own additions to the authorize URL.
	AuthorizeParams url.Values
	// CallbackPort and CallbackPath are where the browser is sent back to on
	// this host. Port 0 means any free port, for a provider that accepts one.
	CallbackPort int
	CallbackPath string
	// ManualRedirectURI is a page of the provider that shows the code to
	// copy. When it is empty a manual login uses the local callback address,
	// and the user copies the address the browser failed to open.
	ManualRedirectURI string
	// JSONTokenRequests says the token endpoint takes a JSON body with the
	// state in it. Otherwise it takes a form.
	JSONTokenRequests bool
	// ChatGPTAccount says the id token names the ChatGPT account that the
	// backend wants in a header.
	ChatGPTAccount bool
}

// ClaudeFlow is the Claude Code login.
func ClaudeFlow() Flow {
	return Flow{
		Provider: "claude",
		ClientID: "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		// The claude.ai surface: a token from the console's authorize page
		// refreshes but is refused by the inference API.
		AuthorizeURL:      "https://claude.com/cai/oauth/authorize",
		TokenURL:          "https://platform.claude.com/v1/oauth/token",
		Scopes:            "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
		RefreshScopes:     "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload",
		AuthorizeParams:   url.Values{"code": {"true"}},
		CallbackPath:      "/callback",
		ManualRedirectURI: "https://platform.claude.com/oauth/code/callback",
		JSONTokenRequests: true,
	}
}

// CodexFlow is the Codex CLI login with a ChatGPT account.
func CodexFlow() Flow {
	return Flow{
		Provider:     "codex",
		ClientID:     "app_EMoamEEZ73f0CkXaXp7hrann",
		AuthorizeURL: "https://auth.openai.com/oauth/authorize",
		TokenURL:     "https://auth.openai.com/oauth/token",
		Scopes:       "openid profile email offline_access api.connectors.read api.connectors.invoke",
		AuthorizeParams: url.Values{
			"id_token_add_organizations": {"true"},
			"codex_cli_simplified_flow":  {"true"},
			"originator":                 {"codex_cli_rs"},
			// Ask for the login page even with a session, so that a second
			// account can be added.
			"prompt": {"login"},
		},
		// The client id is registered with this port only.
		CallbackPort:   1455,
		CallbackPath:   "/auth/callback",
		ChatGPTAccount: true,
	}
}

// Flows returns the built-in flows by provider.
func Flows() map[string]Flow {
	flows := map[string]Flow{}
	for _, f := range []Flow{ClaudeFlow(), CodexFlow()} {
		flows[f.Provider] = f
	}
	return flows
}

// LocalRedirectURI is the redirect address for a callback server on port.
func (f Flow) LocalRedirectURI(port int) string {
	return "http://localhost:" + strconv.Itoa(port) + f.CallbackPath
}

// TokenError is a token endpoint's refusal.
type TokenError struct {
	Status int
	// Detail is the "error" member of the answer, when it is JSON.
	Detail      jsontext.Value
	Description string
}

func (e *TokenError) Error() string {
	text := "token endpoint answered " + strconv.Itoa(e.Status)
	if len(e.Detail) > 0 {
		text += ": " + string(e.Detail)
	}
	if e.Description != "" {
		text += ": " + e.Description
	}
	return text
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
}

// token posts one grant to the token endpoint.
func (f Flow) token(ctx context.Context, client *http.Client, params map[string]string) (tokenResponse, error) {
	var body []byte
	contentType := "application/x-www-form-urlencoded"
	if f.JSONTokenRequests {
		contentType = "application/json"
		var err error
		if body, err = json.Marshal(params); err != nil {
			return tokenResponse{}, err
		}
	} else {
		form := url.Values{}
		for k, v := range params {
			form.Set(k, v)
		}
		body = []byte(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.TokenURL, bytes.NewReader(body))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("%s token request: %w", f.Provider, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about it

	if resp.StatusCode != http.StatusOK {
		var refusal struct {
			Error       jsontext.Value `json:"error"`
			Description string         `json:"error_description"`
		}
		// A refusal that is not JSON, such as a proxy's page, is reported by
		// its status alone.
		_ = json.UnmarshalRead(resp.Body, &refusal)
		return tokenResponse{}, &TokenError{Status: resp.StatusCode, Detail: refusal.Error, Description: refusal.Description}
	}
	var got tokenResponse
	if err := json.UnmarshalRead(resp.Body, &got); err != nil {
		// The text of a JSON error holds a position and never the body.
		return tokenResponse{}, fmt.Errorf("%s token response: %w", f.Provider, err)
	}
	switch {
	case got.AccessToken == "":
		return tokenResponse{}, fmt.Errorf("%s token response has no access_token", f.Provider)
	case got.ExpiresIn <= 0:
		return tokenResponse{}, fmt.Errorf("%s token response has expires_in %d, so the token's expiry is unknown", f.Provider, got.ExpiresIn)
	}
	return got, nil
}

// tokens turns a token response into what is stored. A member the endpoint
// left out keeps its value from prev.
func (f Flow) tokens(got tokenResponse, prev Tokens) (Tokens, error) {
	t := Tokens{
		Provider:         f.Provider,
		AccessToken:      got.AccessToken,
		RefreshToken:     got.RefreshToken,
		ExpiresAtUnixMs:  time.Now().Add(time.Duration(got.ExpiresIn) * time.Second).UnixMilli(),
		Scopes:           strings.Fields(got.Scope),
		ChatGPTAccountID: prev.ChatGPTAccountID,
	}
	if t.RefreshToken == "" {
		t.RefreshToken = prev.RefreshToken
	}
	if len(t.Scopes) == 0 {
		t.Scopes = prev.Scopes
	}
	if t.RefreshToken == "" {
		return Tokens{}, fmt.Errorf("%s token response has no refresh_token", f.Provider)
	}
	if f.ChatGPTAccount {
		if got.IDToken != "" {
			id, err := chatGPTAccountID(got.IDToken)
			if err != nil {
				return Tokens{}, err
			}
			t.ChatGPTAccountID = id
		}
		if t.ChatGPTAccountID == "" {
			return Tokens{}, fmt.Errorf("%s token response names no ChatGPT account", f.Provider)
		}
	}
	return t, nil
}

// chatGPTAccountID reads the account id from an id token's claims. The
// token came from the token endpoint over TLS, so its signature is not checked.
func chatGPTAccountID(idToken string) (string, error) {
	parts := strings.Split(idToken, ".")
	if len(parts) != 3 {
		return "", errors.New("id token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("id token claims: %w", err)
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("id token claims: %w", err)
	}
	return claims.Auth.ChatGPTAccountID, nil
}

// Refresh trades old's refresh token for new tokens.
func (f Flow) Refresh(ctx context.Context, client *http.Client, old Tokens) (Tokens, error) {
	params := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": old.RefreshToken,
		"client_id":     f.ClientID,
	}
	if f.RefreshScopes != "" {
		params["scope"] = f.RefreshScopes
	}
	got, err := f.token(ctx, client, params)
	if err != nil {
		return Tokens{}, err
	}
	return f.tokens(got, old)
}

// Login is one authorization in progress.
type Login struct {
	flow        Flow
	redirectURI string
	verifier    string
	state       string
}

// Begin starts a login that sends the browser back to redirectURI.
func (f Flow) Begin(redirectURI string) *Login {
	return &Login{flow: f, redirectURI: redirectURI, verifier: randomText(), state: randomText()}
}

// randomText is 32 random bytes in the characters a PKCE verifier allows.
func randomText() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails
	return base64.RawURLEncoding.EncodeToString(b)
}

// State is the value the provider must send back with the code.
func (l *Login) State() string { return l.state }

// AuthorizeURL is the page the user opens to approve the login.
func (l *Login) AuthorizeURL() string {
	challenge := sha256.Sum256([]byte(l.verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {l.flow.ClientID},
		"redirect_uri":          {l.redirectURI},
		"scope":                 {l.flow.Scopes},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"state":                 {l.state},
	}
	for k, v := range l.flow.AuthorizeParams {
		q[k] = v
	}
	// The upstream CLIs write a space as %20. Encode writes "+", and a
	// literal plus as %2B, so every "+" here is a space.
	return l.flow.AuthorizeURL + "?" + strings.ReplaceAll(q.Encode(), "+", "%20")
}

// Exchange trades the code the provider sent back for tokens. state is the
// state that came with the code, and must be the login's own.
func (l *Login) Exchange(ctx context.Context, client *http.Client, code, state string) (Tokens, error) {
	if subtle.ConstantTimeCompare([]byte(state), []byte(l.state)) != 1 {
		return Tokens{}, errors.New("the state that came back is not this login's: start the login again and use its own code")
	}
	if code == "" {
		return Tokens{}, errors.New("no authorization code")
	}
	params := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  l.redirectURI,
		"client_id":     l.flow.ClientID,
		"code_verifier": l.verifier,
	}
	if l.flow.JSONTokenRequests {
		params["state"] = l.state
	}
	got, err := l.flow.token(ctx, client, params)
	if err != nil {
		return Tokens{}, err
	}
	return l.flow.tokens(got, Tokens{})
}

// ParsePasted reads what a user copied at the end of a manual login: the
// address the browser was sent to, its query string, or "code#state" as the
// provider's page shows it.
func ParsePasted(input string) (code, state string, err error) {
	input = strings.TrimSpace(input)
	if strings.Contains(input, "code=") {
		query := input
		if u, err := url.Parse(input); err == nil && u.Scheme != "" {
			query = u.RawQuery
		}
		q, err := url.ParseQuery(strings.TrimPrefix(query, "?"))
		if err != nil {
			return "", "", fmt.Errorf("the pasted address has a query that cannot be read: %w", err)
		}
		code, state = q.Get("code"), q.Get("state")
	} else {
		code = input
	}
	if c, s, ok := strings.Cut(code, "#"); ok {
		code, state = c, s
	}
	switch {
	case code == "":
		return "", "", errors.New("nothing pasted holds an authorization code")
	case state == "":
		return "", "", errors.New("the pasted text has a code and no state: paste the whole address, or the whole code#state")
	}
	return code, state, nil
}
