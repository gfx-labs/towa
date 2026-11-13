package anthropic

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"time"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
)

var _ provider.UsageReporter = (*Client)(nil)

// usageWindow is one named window of GET /api/oauth/usage.
type usageWindow struct {
	Utilization *float64  `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

// usageResponse is the part of GET /api/oauth/usage that is read, as the
// endpoint answered on 2026-11-12. A window the account has no use in is
// null.
type usageResponse struct {
	FiveHour       *usageWindow `json:"five_hour"`
	SevenDay       *usageWindow `json:"seven_day"`
	SevenDayOpus   *usageWindow `json:"seven_day_opus"`
	SevenDaySonnet *usageWindow `json:"seven_day_sonnet"`
	// Limits repeats the windows above and adds the ones that apply to one
	// model, which have no key of their own.
	Limits []struct {
		Kind     string    `json:"kind"`
		Percent  *float64  `json:"percent"`
		ResetsAt time.Time `json:"resets_at"`
		Scope    *struct {
			Model *struct {
				DisplayName string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

// Usage returns the subscription usage of an OAuth account, from GET
// /api/oauth/usage. An API key has no subscription and gets
// provider.ErrNoUsage.
func (c *Client) Usage(ctx context.Context, cred auth.Credential) (provider.Usage, error) {
	if cred.Scheme != auth.SchemeOAuth {
		return provider.Usage{}, provider.ErrNoUsage
	}
	header, perr := c.header(cred, Thinking{})
	if perr != nil {
		return provider.Usage{}, perr
	}
	header.Del("Content-Type")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.BaseURL+"/api/oauth/usage", http.NoBody)
	if err != nil {
		return provider.Usage{}, invalidRequest("build request: " + err.Error())
	}
	req.Header = header
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return provider.Usage{}, provider.TransportError(ctx, "send request", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body that was read
	if resp.StatusCode != http.StatusOK {
		return provider.Usage{}, c.statusError(ctx, resp)
	}
	var body usageResponse
	if err := json.UnmarshalRead(resp.Body, &body); err != nil {
		return provider.Usage{}, provider.TransportError(ctx, "read the usage", err)
	}
	var usage provider.Usage
	for _, window := range []struct {
		name string
		w    *usageWindow
	}{
		{"five_hour", body.FiveHour},
		{"seven_day", body.SevenDay},
		{"seven_day_opus", body.SevenDayOpus},
		{"seven_day_sonnet", body.SevenDaySonnet},
	} {
		if window.w != nil && window.w.Utilization != nil {
			usage.Limits = append(usage.Limits, provider.UsageLimit{Name: window.name, UsedPercent: *window.w.Utilization, ResetsAt: window.w.ResetsAt})
		}
	}
	for _, limit := range body.Limits {
		if limit.Percent == nil || limit.Scope == nil || limit.Scope.Model == nil || limit.Scope.Model.DisplayName == "" {
			continue
		}
		usage.Limits = append(usage.Limits, provider.UsageLimit{
			Name: limit.Kind + " " + limit.Scope.Model.DisplayName, UsedPercent: *limit.Percent, ResetsAt: limit.ResetsAt,
		})
	}
	return usage, nil
}
