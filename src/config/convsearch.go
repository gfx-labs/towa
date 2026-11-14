package config

// ConversationSearch bounds reads of the full stored conversation. A search
// covers descendants by default. The four sizes are jcode's at 8e11eb23e,
// conversation_search.rs:135,306,310,189,201.
type ConversationSearch struct {
	Enabled             bool   `name:"enabled" default:"true" help:"Give the model the tool that searches the stored conversation. On by default."`
	Scope               string `name:"scope" default:"descendants" validate:"oneof=self descendants store" help:"Sessions a search covers: self, descendants or store."`
	MaxHits             int    `name:"max_hits" default:"10" validate:"gt=0" help:"Most hits one search returns."`
	SnippetContextBytes int    `name:"snippet_context_bytes" default:"50" validate:"gt=0" help:"Bytes shown on each side of a hit."`
	MessageBytes        int    `name:"message_bytes" default:"1000" validate:"gt=0" help:"Most bytes of a message that a read returns."`
	ToolResultBytes     int    `name:"tool_result_bytes" default:"200" validate:"gt=0" help:"Most bytes of a tool result that a read returns."`
	// MaxBlobBytes is zero by default: stored blobs are opt-in because a
	// store can hold large binary outputs.
	MaxBlobBytes int64 `name:"max_blob_bytes" default:"0" validate:"gte=0" help:"Largest stored blob a search reads. 0 reads none."`
}

func (c ConversationSearch) validate() error {
	return validateFields("conversation_search", c)
}
