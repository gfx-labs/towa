// Package hostedsearch is web search run by the model's provider: the tool
// definition each API takes, and a reading of the items a search leaves in
// the transcript. Those items are stored whole and only their own provider
// takes them back, so this package also gives the text that stands for one
// in a request to another provider.
package hostedsearch

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Name is the provider's name for the tool, on both APIs.
const Name = "web_search"

// anthropicVersion is the Messages API's first web search tool. Later
// versions call through code execution unless told otherwise, which a model
// without programmatic tool calling refuses (jcode,
// crates/jcode-provider-anthropic-runtime/src/native_web_search.rs at
// 037163c7b).
const anthropicVersion = "web_search_20250305"

// Kinds of the items a hosted search leaves, as transcript.Native.Kind.
const (
	kindAnthropicUse    = "server_tool_use"
	kindAnthropicResult = "web_search_tool_result"
	kindOpenAICall      = "web_search_call"
)

// Tool returns the hosted search tool for a chain entry, and false when
// the entry's provider has none. Only the two built-in providers have it: a
// configured endpoint that speaks the same API may not implement it.
func Tool(cfg config.WebSearch, providerID, model string) (provider.Tool, bool, error) {
	if !cfg.Enabled || !cfg.Hosted {
		return provider.Tool{}, false, nil
	}
	var def map[string]any
	var api provider.NativeAPI
	switch providerID {
	case config.ProviderClaude:
		def, api = map[string]any{"type": anthropicVersion, "name": Name}, provider.NativeAPIMessages
		if cfg.HostedMaxUses > 0 {
			def["max_uses"] = cfg.HostedMaxUses
		}
	case config.ProviderCodex:
		// A model with codex in its name refuses hosted tools it does not
		// know (jcode, crates/jcode-provider-openai-runtime/src/native_web_search.rs).
		if strings.Contains(strings.ToLower(model), "codex") {
			return provider.Tool{}, false, nil
		}
		def, api = map[string]any{"type": Name}, provider.NativeAPIResponses
	default:
		return provider.Tool{}, false, nil
	}
	raw, err := json.Marshal(def, json.Deterministic(true))
	if err != nil {
		return provider.Tool{}, false, fmt.Errorf("hostedsearch: %w", err)
	}
	return provider.Tool{Name: Name, Native: raw, NativeAPI: api}, true, nil
}

// Event is what one stored item says about a search.
type Event struct {
	// ID is the provider's id for the search, shared by its start and end.
	ID string
	// Query is what was searched for, or the page that was opened. It is
	// empty on an Anthropic result, which does not repeat it.
	Query string
	// Started says the item begins a search and Done that it ends one. An
	// OpenAI item is both.
	Started, Done bool
	Failed        bool
	// Output is the result as text: what was done and the pages found.
	Output string
}

type source struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

// Read returns what a native item says about a hosted search, and false
// for an item that is not one.
func Read(item transcript.Item) (Event, bool) {
	if item.Native == nil || len(item.Opaque) == 0 {
		return Event{}, false
	}
	switch item.Native.Kind {
	case kindAnthropicUse:
		var use struct {
			ID    string `json:"id"`
			Name  string `json:"name"`
			Input struct {
				Query string `json:"query"`
			} `json:"input"`
		}
		if json.Unmarshal(item.Opaque, &use) != nil || use.ID == "" || use.Name != Name {
			return Event{}, false
		}
		return Event{ID: use.ID, Query: use.Input.Query, Started: true}, true
	case kindAnthropicResult:
		var result struct {
			ToolUseID string         `json:"tool_use_id"`
			Content   jsontext.Value `json:"content"`
		}
		if json.Unmarshal(item.Opaque, &result) != nil || result.ToolUseID == "" {
			return Event{}, false
		}
		ev := Event{ID: result.ToolUseID, Done: true}
		var found []source
		if result.Content.Kind() == '[' && json.Unmarshal(result.Content, &found) == nil {
			ev.Output = list(found)
			return ev, true
		}
		// Anything but a list of results is the API's error object.
		var failure struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.Unmarshal(result.Content, &failure) //nolint:errcheck // an unreadable error is still a failed search
		ev.Failed, ev.Output = true, "Web search failed: "+failure.ErrorCode
		return ev, true
	case kindOpenAICall:
		var call struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Action struct {
				Type    string   `json:"type"`
				Query   string   `json:"query"`
				Queries []string `json:"queries"`
				URL     string   `json:"url"`
				Pattern string   `json:"pattern"`
				Sources []source `json:"sources"`
			} `json:"action"`
		}
		if json.Unmarshal(item.Opaque, &call) != nil || call.ID == "" {
			return Event{}, false
		}
		ev := Event{ID: call.ID, Started: true, Done: true, Failed: call.Status == "failed"}
		switch {
		case call.Action.Query != "":
			ev.Query = call.Action.Query
		case len(call.Action.Queries) > 0:
			ev.Query = strings.Join(call.Action.Queries, ", ")
		default:
			ev.Query = call.Action.URL
		}
		switch {
		case ev.Failed:
			ev.Output = "Web search failed"
		case call.Action.Type == "open_page":
			ev.Output = "Opened page: " + ev.Query
		case call.Action.Type == "find_in_page" || call.Action.Type == "find":
			ev.Output = fmt.Sprintf("Searched page %s for: %s", call.Action.URL, call.Action.Pattern)
		default:
			ev.Output = "Searched: " + ev.Query
		}
		if len(call.Action.Sources) > 0 {
			ev.Output += "\n\n" + list(call.Action.Sources)
		}
		return ev, true
	}
	return Event{}, false
}

func list(found []source) string {
	var b strings.Builder
	n := 0
	for _, s := range found {
		if s.URL == "" {
			continue
		}
		n++
		if strings.TrimSpace(s.Title) == "" {
			fmt.Fprintf(&b, "%d. %s\n", n, s.URL)
			continue
		}
		fmt.Fprintf(&b, "%d. %s\n   %s\n", n, s.Title, s.URL)
	}
	if n == 0 {
		return "No results"
	}
	return strings.TrimRight(b.String(), "\n")
}

// standInNote tells the model the list is not what it read at the time, so
// that it does not take the titles for the full results.
const standInNote = "(summary of an earlier provider-side search; page contents are no longer available)"

// Portable returns items with every hosted search item that the request
// cannot carry replaced by text. A search item goes back as it is only to
// the provider, model and account that made it, and only when the request
// declares the hosted tool. Other items are returned unchanged.
func Portable(items []transcript.Item, providerName string, origin transcript.Origin, declared bool) []transcript.Item {
	var out []transcript.Item
	queries := map[string]string{}
	for i, item := range items {
		ev, ok := Read(item)
		if !ok || (declared && item.OpaqueCompatible(providerName, origin)) {
			if out != nil {
				out = append(out, item)
			}
			continue
		}
		if out == nil {
			out = append(make([]transcript.Item, 0, len(items)), items[:i]...)
		}
		if ev.Started && ev.Query != "" {
			queries[ev.ID] = ev.Query
		}
		if !ev.Done {
			continue
		}
		head := "[" + Name + " results]"
		if query := queries[ev.ID]; query != "" {
			head = fmt.Sprintf("[%s for %q]", Name, query)
		}
		out = append(out, transcript.Item{Text: &transcript.Text{Text: head + " " + standInNote + "\n" + ev.Output}})
	}
	if out == nil {
		return items
	}
	return out
}
