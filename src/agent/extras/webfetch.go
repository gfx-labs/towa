package extras

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"time"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/webfetch"
	"github.com/gfx-labs/towa/src/transcript"
)

// WebFetch fetches text or converts HTML to Markdown using the supplied client.
// The client comes from src/support/httpclient. Timeout and format are configuration,
// not call arguments, because the capability has no per-call overrides.
func WebFetch(cfg config.WebFetch, client *http.Client) (tools.Tool, error) {
	if !cfg.Enabled {
		return tools.Tool{}, fmt.Errorf("webfetch: %w", ErrDisabled)
	}
	if client == nil {
		return tools.Tool{}, errors.New("webfetch: no HTTP client")
	}
	t := newTool(NameWebFetch, "Fetch an http or https URL and return its content. An HTML page comes back as Markdown, and a text, JSON or XML body as it was served. Long content is cut, with a note saying how much was left out. Other content types fail.", acp.ToolKindFetch, []byte(`{"type":"object","required":["url"],"properties":{"intent":{"type":"string"},"url":{"type":"string","description":"The address, starting with http:// or https://."}}}`))
	t.Summary = "Fetch a URL and return its content"
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		res, err := webfetch.Fetch(ctx, client, cfg, webfetch.Input{URL: in.URL})
		if err != nil {
			e, ok := errors.AsType[*webfetch.Error](err)
			if !ok {
				return tools.Result{}, err
			}
			return textResult(transcript.OutcomeFailed, fetchError(e)), nil
		}
		text := fmt.Sprintf("Fetched %s (%d bytes)\n\n%s", res.URL, len(res.Text)+int(res.OmittedBytes), res.Text)
		if res.OmittedBytes > 0 {
			text += fmt.Sprintf("\n\n(output truncated to %d of %d bytes; fetch a more specific URL or anchor for the rest)", len(res.Text), int64(len(res.Text))+res.OmittedBytes)
		}
		return textResult(transcript.OutcomeCompleted, text), nil
	}
	return t, nil
}

func fetchError(e *webfetch.Error) string {
	switch e.Kind {
	case webfetch.KindInvalidURL:
		return "Invalid URL: " + e.URL
	case webfetch.KindScheme:
		return "URL must start with http:// or https://"
	case webfetch.KindTimeout:
		return fmt.Sprintf("Fetch timed out after %s", time.Duration(e.Limit))
	case webfetch.KindNetwork:
		return fmt.Sprintf("Fetch failed for %s: %v", e.URL, e.Err)
	case webfetch.KindStatus:
		return fmt.Sprintf("HTTP error: %d", e.Status)
	case webfetch.KindTooLarge:
		return fmt.Sprintf("Response too large (max %d bytes)", e.Limit)
	case webfetch.KindContentType:
		return "Unsupported content type: " + e.ContentType
	case webfetch.KindParse:
		return "Could not parse the HTML response."
	default:
		return e.Error()
	}
}
