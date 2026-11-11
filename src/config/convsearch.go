package config

// ConversationSearch bounds reads of the full stored conversation.
type ConversationSearch struct {
	Enabled             bool   `koanf:"enabled"`
	Scope               string `koanf:"scope" validate:"oneof=self descendants store"`
	MaxHits             int    `koanf:"max_hits" validate:"gt=0"`
	SnippetContextBytes int    `koanf:"snippet_context_bytes" validate:"gt=0"`
	MessageBytes        int    `koanf:"message_bytes" validate:"gt=0"`
	ToolResultBytes     int    `koanf:"tool_result_bytes" validate:"gt=0"`
	MaxBlobBytes        int64  `koanf:"max_blob_bytes" validate:"gte=0"`
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
	return validateFields("conversation_search", c)
}
