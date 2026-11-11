package auth

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DeviceFlow is a provider's device code login: the user types a code into a
// page on any device, and the harness asks until it is approved. The zero
// value says the provider has none.
type DeviceFlow struct {
	// UserCodeURL issues the code, and PollURL answers once it is approved.
	UserCodeURL string
	PollURL     string
	// VerificationURL is the page the user types the code into.
	VerificationURL string
	// RedirectURI is what the code exchange names. No browser is sent there.
	RedirectURI string
	// Lifetime is how long the provider keeps a code.
	Lifetime time.Duration
}

// deviceLogin shows a code to type into the provider's page and waits for
// its approval. The requests are those of the Codex CLI
// (codex-rs/login/src/device_code_auth.rs).
func deviceLogin(ctx context.Context, opts LoginOptions) (Tokens, error) {
	flow, d := opts.Flow, opts.Flow.Device
	if d.UserCodeURL == "" {
		return Tokens{}, fmt.Errorf("%s has no device code login", flow.Provider)
	}
	var issued struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		// The endpoint has used both spellings, and upstream reads either.
		UserCodeOld string `json:"usercode"`
		// Seconds, as text.
		Interval string `json:"interval"`
	}
	status, err := postJSON(ctx, opts, d.UserCodeURL, map[string]string{"client_id": flow.ClientID}, &issued)
	switch {
	case err != nil:
		return Tokens{}, err
	case status != http.StatusOK:
		return Tokens{}, fmt.Errorf("%s device login: the code request was answered %d", flow.Provider, status)
	}
	if issued.UserCode == "" {
		issued.UserCode = issued.UserCodeOld
	}
	seconds, err := strconv.ParseUint(strings.TrimSpace(issued.Interval), 10, 31)
	switch {
	case issued.DeviceAuthID == "" || issued.UserCode == "":
		return Tokens{}, fmt.Errorf("%s device login: the answer holds no code", flow.Provider)
	case err != nil || seconds == 0:
		// Upstream asks without a pause when the interval is missing.
		return Tokens{}, fmt.Errorf("%s device login: the answer does not say how often to ask", flow.Provider)
	}
	interval := time.Duration(seconds) * time.Second

	if _, err := fmt.Fprintf(opts.Out,
		"Open this page in a browser on any device and sign in:\n\n%s\n\nEnter this one-time code there (it works for %s):\n\n%s\n\n"+
			"Continue only if you started this login. If a website or another person gave you the code, stop here.\n",
		d.VerificationURL, d.Lifetime, issued.UserCode); err != nil {
		return Tokens{}, err
	}

	var approved struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	poll := map[string]string{"device_auth_id": issued.DeviceAuthID, "user_code": issued.UserCode}
	expires := time.Now().Add(d.Lifetime)
	for {
		status, err := postJSON(ctx, opts, d.PollURL, poll, &approved)
		if err != nil {
			return Tokens{}, err
		}
		if status == http.StatusOK {
			break
		}
		// Both mean the code is not approved yet.
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return Tokens{}, fmt.Errorf("%s device login: the approval request was answered %d", flow.Provider, status)
		}
		if time.Now().Add(interval).After(expires) {
			return Tokens{}, fmt.Errorf("%s device login: the code was not approved within %s", flow.Provider, d.Lifetime)
		}
		wait := time.NewTimer(interval)
		select {
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return Tokens{}, context.Cause(ctx)
		}
	}
	if approved.AuthorizationCode == "" || approved.CodeVerifier == "" {
		return Tokens{}, fmt.Errorf("%s device login: the approval holds no authorization code", flow.Provider)
	}

	ctx, cancel := context.WithTimeout(ctx, opts.TokenTimeout)
	defer cancel()
	got, err := flow.token(ctx, opts.HTTPClient, map[string]string{
		"grant_type":    "authorization_code",
		"code":          approved.AuthorizationCode,
		"redirect_uri":  d.RedirectURI,
		"client_id":     flow.ClientID,
		"code_verifier": approved.CodeVerifier,
	})
	if err != nil {
		return Tokens{}, err
	}
	return flow.tokens(got, Tokens{})
}

// postJSON posts body and decodes a 200 answer into out. Any other status
// is returned undecoded.
func postJSON(ctx context.Context, opts LoginOptions, url string, body, out any) (status int, err error) {
	ctx, cancel := context.WithTimeout(ctx, opts.TokenTimeout)
	defer cancel()
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("%s device login: %w", opts.Flow.Provider, err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about it
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	if err := json.UnmarshalRead(resp.Body, out); err != nil {
		// The text of a JSON error holds a position and never the body.
		return 0, errors.Join(fmt.Errorf("%s device login: the answer is not the expected JSON", opts.Flow.Provider), err)
	}
	return resp.StatusCode, nil
}
