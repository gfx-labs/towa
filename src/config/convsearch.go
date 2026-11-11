package config

import "fmt"

// ConversationSearch bounds reads of the full stored conversation.
type ConversationSearch struct {
	Enabled             bool   `koanf:"enabled"`
	Scope               string `koanf:"scope"`
	MaxHits             int    `koanf:"max_hits"`
	SnippetContextBytes int    `koanf:"snippet_context_bytes"`
	MessageBytes        int    `koanf:"message_bytes"`
	ToolResultBytes     int    `koanf:"tool_result_bytes"`
	MaxBlobBytes        int64  `koanf:"max_blob_bytes"`
}

var conversationSearchDefaults = map[string]any{
	// docs/design.md: conversation search is available, descendants is the default scope.
	"conversation_search.enabled": true,
	"conversation_search.scope":   "descendants",
	// jcode 8e11eb23e, conversation_search.rs:135,306,310,189,201.
	"conversation_search.max_hits":              10,
	"conversation_search.snippet_context_bytes": 50,
	"conversation_search.message_bytes":         1000,
	"conversation_search.tool_result_bytes":     200,
	// Stored blobs are opt-in because a store can hold large binary outputs.
	"conversation_search.max_blob_bytes": 0,
}

func (c ConversationSearch) validate() error {
	switch c.Scope {
	case "self", "descendants", "store":
	default:
		return fmt.Errorf("conversation_search.scope: unknown scope %q", c.Scope)
	}
	for key, n := range map[string]int{"max_hits": c.MaxHits, "snippet_context_bytes": c.SnippetContextBytes, "message_bytes": c.MessageBytes, "tool_result_bytes": c.ToolResultBytes} {
		if n <= 0 {
			return fmt.Errorf("conversation_search.%s: %d is not positive", key, n)
		}
	}
	if c.MaxBlobBytes < 0 {
		return fmt.Errorf("conversation_search.max_blob_bytes: %d is negative", c.MaxBlobBytes)
	}
	return nil
}
