package extras

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	acp "github.com/gfx-labs/go-acp"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/websearch"
	"github.com/gfx-labs/towa/src/transcript"
)

// SearchSummary is a web search tool's line in the prompt's list of tools,
// for this package's tool and for a provider's own.
const SearchSummary = "Search the web"

// searchGuidance is the prompt's rule about a web search tool.
func searchGuidance(name string) string {
	return "What you know stops at your training. Check a fact that may have changed since, such as a current version or API, with " + name + "."
}

// HostedSearchGuidance is the line for a provider's own search tool, which
// takes the place of this package's tool in a request.
func HostedSearchGuidance(name string) string { return searchGuidance(name) }

// WebSearch searches the web with the supplied client, which comes from
// src/support/httpclient.
func WebSearch(cfg config.WebSearch, client *http.Client) (tools.Tool, error) {
	if !cfg.Enabled {
		return tools.Tool{}, fmt.Errorf("websearch: %w", ErrDisabled)
	}
	if client == nil {
		return tools.Tool{}, errors.New("websearch: no HTTP client")
	}
	t := newTool(NameWebSearch, "Search the web. Returns titles, URLs and snippets. Fetch a result's URL to read the page.", acp.ToolKindSearch,
		[]byte(`{"type":"object","required":["query"],"properties":{"intent":{"type":"string"},"query":{"type":"string","description":"Search query."},"num_results":{"type":"integer","description":"How many results to return. Omit for the configured default."}}}`))
	t.Summary = SearchSummary
	t.Guidance = []string{searchGuidance(NameWebSearch)}
	t.Run = func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var in struct {
			Query      string `json:"query"`
			NumResults int    `json:"num_results"`
		}
		if err := json.Unmarshal(call.Input, &in); err != nil {
			return tools.Result{}, err
		}
		results, err := websearch.Search(ctx, client, cfg, websearch.Input{Query: in.Query, Results: in.NumResults})
		if err != nil {
			e, ok := errors.AsType[*websearch.Error](err)
			if !ok {
				return tools.Result{}, err
			}
			return textResult(transcript.OutcomeFailed, searchError(e)), nil
		}
		if len(results) == 0 {
			return textResult(transcript.OutcomeCompleted, "No results found for: "+in.Query), nil
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Search results for: %s\n", in.Query)
		for i, r := range results {
			fmt.Fprintf(&out, "\n%d. %s\n   %s\n", i+1, r.Title, r.URL)
			if r.Snippet != "" {
				fmt.Fprintf(&out, "   %s\n", r.Snippet)
			}
		}
		return textResult(transcript.OutcomeCompleted, out.String()), nil
	}
	return t, nil
}

func searchError(e *websearch.Error) string {
	switch e.Kind {
	case websearch.KindEmptyQuery:
		return "The query is empty."
	case websearch.KindCount:
		return fmt.Sprintf("num_results must be between 1 and %d.", e.Limit)
	case websearch.KindTimeout:
		return fmt.Sprintf("Search timed out after %s", time.Duration(e.Limit))
	case websearch.KindNetwork:
		return fmt.Sprintf("Search failed: %v", e.Err)
	case websearch.KindStatus:
		return fmt.Sprintf("Search failed with HTTP status %d", e.Status)
	case websearch.KindTooLarge:
		return fmt.Sprintf("Search response too large (max %d bytes)", e.Limit)
	case websearch.KindBlocked:
		return "The search engine answered with a bot challenge and no results. It does this after many searches from one address. Wait before searching again."
	case websearch.KindParse:
		return "Could not parse the search results."
	default:
		return e.Error()
	}
}
