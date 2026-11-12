package openai

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
)

var _ provider.UsageReporter = (*Client)(nil)

// codexPath ends the Codex backend's base URL. The usage endpoint is beside
// it: .../backend-api/codex and .../backend-api/wham/usage.
const codexPath = "/codex"

// usageWindow is one window of the ChatGPT usage endpoint.
type usageWindow struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// Usage returns the subscription usage of an OAuth account, from the
// ChatGPT backend's wham/usage, as it answered on 2026-11-12. An API key
// has no subscription and gets provider.ErrNoUsage.
func (c *Client) Usage(ctx context.Context, cred auth.Credential) (provider.Usage, error) {
	ep, perr := c.endpoint(cred)
	if perr != nil {
		return provider.Usage{}, perr
	}
	if !ep.codex {
		return provider.Usage{}, provider.ErrNoUsage
	}
	backend, ok := strings.CutSuffix(ep.base, codexPath)
	if !ok {
		return provider.Usage{}, invalidRequest(fmt.Sprintf("the Codex base URL %q does not end in %s, so the usage endpoint beside it is not known", ep.base, codexPath))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, backend+"/wham/usage", http.NoBody)
	if err != nil {
		return provider.Usage{}, invalidRequest("build request: " + err.Error())
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
		return provider.Usage{}, transportError(ctx, "send request", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body that was read
	if resp.StatusCode != http.StatusOK {
		return provider.Usage{}, c.statusError(ctx, resp)
	}
	var body struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			PrimaryWindow   *usageWindow `json:"primary_window"`
			SecondaryWindow *usageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return provider.Usage{}, transportError(ctx, "read the usage", err)
	}
	usage := provider.Usage{Plan: body.PlanType}
	if body.RateLimit != nil {
		for _, w := range []*usageWindow{body.RateLimit.PrimaryWindow, body.RateLimit.SecondaryWindow} {
			if w == nil {
				continue
			}
			limit := provider.UsageLimit{Name: windowName(w.LimitWindowSeconds), UsedPercent: w.UsedPercent}
			if w.ResetAt != 0 {
				limit.ResetsAt = time.Unix(w.ResetAt, 0)
			}
			usage.Limits = append(usage.Limits, limit)
		}
	}
	return usage, nil
}

// windowName names a window by its length: "7d", "5h", or the duration as
// Go writes it when it is neither whole days nor whole hours.
func windowName(seconds int64) string {
	length := time.Duration(seconds) * time.Second
	const day = 24 * time.Hour
	switch {
	case length > 0 && length%day == 0:
		return fmt.Sprintf("%dd", length/day)
	case length > 0 && length%time.Hour == 0:
		return fmt.Sprintf("%dh", length/time.Hour)
	}
	return length.String()
}
