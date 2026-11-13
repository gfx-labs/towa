package anthropic

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/url"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
)

var _ provider.ModelLister = (*Client)(nil)

// modelsPage is one page of GET /v1/models, as read on 2026-11-12.
type modelsPage struct {
	Data []struct {
		ID             string `json:"id"`
		MaxInputTokens int64  `json:"max_input_tokens"`
		MaxTokens      int64  `json:"max_tokens"`
		Capabilities   struct {
			Thinking struct {
				Types struct {
					Adaptive struct {
						Supported *bool `json:"supported"`
					} `json:"adaptive"`
				} `json:"types"`
			} `json:"thinking"`
		} `json:"capabilities"`
	} `json:"data"`
	HasMore bool   `json:"has_more"`
	LastID  string `json:"last_id"`
}

// Models returns the models the credential's account can use, from GET
// /v1/models. It asks for the pages the endpoint's own page size gives.
func (c *Client) Models(ctx context.Context, cred auth.Credential) ([]provider.ModelInfo, error) {
	var out []provider.ModelInfo
	after := ""
	for {
		page, perr := c.modelsPage(ctx, cred, after)
		if perr != nil {
			return nil, perr
		}
		for _, m := range page.Data {
			adaptive := m.Capabilities.Thinking.Types.Adaptive.Supported
			out = append(out, provider.ModelInfo{
				ID: m.ID, ContextWindow: m.MaxInputTokens, MaxOutputTokens: m.MaxTokens,
				NoAdaptiveThinking: adaptive != nil && !*adaptive,
			})
		}
		if !page.HasMore {
			return out, nil
		}
		if page.LastID == "" || page.LastID == after {
			return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "the model list says there is another page and does not say where it starts"}
		}
		after = page.LastID
	}
}

func (c *Client) modelsPage(ctx context.Context, cred auth.Credential, after string) (*modelsPage, *provider.Error) {
	header, perr := c.header(cred, Thinking{})
	if perr != nil {
		return nil, perr
	}
	header.Set("Accept", "application/json")
	header.Del("Content-Type")
	address := c.cfg.BaseURL + "/v1/models"
	if after != "" {
		address += "?" + url.Values{"after_id": {after}}.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, http.NoBody)
	if err != nil {
		return nil, invalidRequest("build request: " + err.Error())
	}
	req.Header = header
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, provider.TransportError(ctx, "send request", err)
	}
	defer resp.Body.Close() //nolint:errcheck // nothing to do about a close error on a body that was read
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError(ctx, resp)
	}
	var page modelsPage
	if err := json.UnmarshalRead(resp.Body, &page); err != nil {
		return nil, provider.TransportError(ctx, "read the model list", err)
	}
	return &page, nil
}
