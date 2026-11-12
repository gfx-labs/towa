package openai

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

type compactBody struct {
	Model        string `json:"model"`
	Instructions string `json:"instructions,omitzero"`
	Input        []any  `json:"input"`
	Store        bool   `json:"store"`
}

// Compacted is the result of Compact.
type Compacted struct {
	// Items stand for the request's items in later requests to the same
	// endpoint. Each is a Native item holding one output item of the
	// compact call unchanged. One of them has the kind "compaction" and
	// carries the encrypted summary. Another provider cannot read them.
	Items []transcript.Item
	// Usage is nil when the endpoint reported none.
	Usage *transcript.Usage
}

// Compact asks the endpoint to compact a conversation, with the compact
// call of the Responses API. It reads Model, Credential, System and Items
// of req. Every error is a *provider.Error.
func (c *Client) Compact(ctx context.Context, req provider.Request) (*Compacted, error) {
	if req.Credential.Account == "" {
		return nil, invalidRequest("the request credential names no account")
	}
	ep, perr := c.endpoint(req.Credential)
	if perr != nil {
		return nil, perr
	}
	if req.Model == "" {
		return nil, invalidRequest("the request names no model")
	}
	origin := transcript.Origin{Model: req.Model, Account: req.Credential.Account}
	input, perr := buildInput(ep.provider, origin, req.Items)
	if perr != nil {
		return nil, perr
	}
	buf := newPooledBody()
	defer buf.release()
	body := compactBody{Model: req.Model, Instructions: buildInstructions(req.System), Input: input}
	if err := json.MarshalWrite(buf.buf, body); err != nil {
		return nil, invalidRequest("encode request: " + err.Error())
	}
	resp, perr := c.post(ctx, ep.base+"/responses/compact", c.header(ep, req), buf, "application/json")
	if perr != nil {
		return nil, perr
	}
	defer resp.Body.Close() //nolint:errcheck // the body has been read or the read already failed

	var out responseBody
	if err := json.UnmarshalRead(resp.Body, &out); err != nil {
		if ctx.Err() != nil {
			return nil, transportError(ctx, "read compact response", err)
		}
		return nil, &provider.Error{
			Class: provider.Retry, Reason: provider.ReasonNetwork, Status: resp.StatusCode,
			Message: "the compact response is not the JSON this client reads", Err: err,
		}
	}
	result := &Compacted{Items: make([]transcript.Item, 0, len(out.Output))}
	found := false
	for i, raw := range out.Output {
		item, perr := decodeItem(raw)
		if perr != nil {
			return nil, perr
		}
		if item.Type == "" {
			return nil, protocolError(fmt.Sprintf("output item %d of the compact response has no type", i))
		}
		found = found || (item.Type == typeCompaction && item.EncryptedContent != "")
		result.Items = append(result.Items, transcript.Item{
			Native: &transcript.Native{Kind: item.Type}, Provider: ep.provider, Opaque: raw.Clone(),
			Origin: &transcript.Origin{Model: req.Model, Account: req.Credential.Account},
		})
	}
	if out.Usage != nil {
		if result.Usage, perr = convertUsage(out.Usage, req.Model, req.Credential.Account); perr != nil {
			return nil, perr
		}
	}
	if !found {
		// Without the item the conversation would be replaced by nothing.
		return nil, protocolError("the compact response holds no compaction item")
	}
	return result, nil
}
