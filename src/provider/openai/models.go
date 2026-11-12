package openai

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
)

var _ provider.ModelLister = (*Client)(nil)

// codexClientVersion is the client_version the Codex backend's model list
// requires: it answers HTTP 400 without one, and lists only the models
// whose minimal_client_version is not above it (none at 0.1.0, on
// 2026-11-12). jcode 037163c7b sends this value.
const codexClientVersion = "1.0.0"

// Models returns the models an OAuth account can use, from the Codex
// backend's model list, which gives a context window and no output limit.
// The API key endpoint's list gives neither figure, so an API key gets
// provider.ErrNoModelList.
func (c *Client) Models(ctx context.Context, cred auth.Credential) ([]provider.ModelInfo, error) {
	ep, perr := c.endpoint(cred)
	if perr != nil {
		return nil, perr
	}
	if !ep.codex {
		return nil, provider.ErrNoModelList
	}
	address := ep.base + "/models?" + url.Values{"client_version": {codexClientVersion}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return nil, invalidRequest("build request: " + err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("originator", Originator)
	if c.cfg.UserAgent != "" {
		req.Header.Set("User-Agent", c.cfg.UserAgent)
	}
	if cred.ChatGPTAccountID != "" {
		req.Header.Set("chatgpt-account-id", cred.ChatGPTAccountID)
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, transportError(ctx, "send request", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body that was read
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(ctx, resp)
	}
	var list struct {
		Models []struct {
			Slug          string `json:"slug"`
			ContextWindow int64  `json:"context_window"`
		} `json:"models"`
	}
	if err := json.UnmarshalRead(resp.Body, &list); err != nil {
		return nil, transportError(ctx, "read the model list", err)
	}
	out := make([]provider.ModelInfo, len(list.Models))
	for i, m := range list.Models {
		out[i] = provider.ModelInfo{ID: m.Slug, ContextWindow: m.ContextWindow}
	}
	return out, nil
}
