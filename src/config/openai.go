package config

// OpenAI configures the Responses API client, for both of its endpoints:
// an API key against api.openai.com or a configured base URL, and a ChatGPT
// login against the Codex backend.
type OpenAI struct {
	// Transport is how a request reaches the endpoint: "sse" posts it and
	// reads server-sent events, "websocket" keeps one connection per
	// conversation and sends only the new input on it.
	Transport string `koanf:"transport" validate:"oneof=sse websocket"`
	// MaxIdleSockets is how many WebSocket connections are kept open
	// between requests, one per conversation. It has no default and must be
	// set when Transport is "websocket". It is not read otherwise.
	MaxIdleSockets int `koanf:"max_idle_sockets" validate:"required_if=Transport websocket,gte=0"`
	// CompactThreshold asks the server to compact a conversation itself
	// once it holds this many tokens. Zero leaves server-side compaction
	// off.
	CompactThreshold int64 `koanf:"compact_threshold" validate:"gte=0"`
	// ParallelToolCalls lets the model ask for several tools in one
	// response.
	ParallelToolCalls bool `koanf:"parallel_tool_calls"`
	// ErrorBodyBytes is how much of an error response is read. A longer
	// one is cut, and the error's message says so.
	ErrorBodyBytes int64 `koanf:"error_body_bytes" validate:"gt=0"`
}

// Transports of the Responses API client.
const (
	OpenAITransportSSE       = "sse"
	OpenAITransportWebSocket = "websocket"
)

var openaiDefaults = map[string]any{
	// sse is the transport with recordings replayed in the test suite. The
	// WebSocket transport has never run against a real server
	// (docs/notes/2026-11-11-provider-openai.md), so it is opt-in.
	"openai.transport": OpenAITransportSSE,
	// Zero is "not set", not a number of sockets: validate refuses it when
	// the transport is websocket. No source gives a number. It depends on
	// how many sessions one process serves.
	"openai.max_idle_sockets": 0,
	// Off. The right threshold is a fraction of the model's context window,
	// which the compaction settings will own when compaction is built.
	"openai.compact_threshold": 0,
	// jcode sends parallel_tool_calls false on every Responses request
	// (crates/jcode-provider-openai-runtime/src/lib.rs,
	// build_response_request). openai/codex ce0323f2 decides it per model.
	"openai.parallel_tool_calls": false,
	// 1 MiB, the same value as openai.DefaultErrorBodyBytes. It is a choice
	// and not a measurement: jcode reads an error body without a bound. The
	// error bodies seen so far are 117 bytes (Ollama's unknown model, in
	// src/provider/openai/testdata) and about 190 bytes (the usage limit
	// body quoted in jcode's openai_rate_limit_format.rs). The bound is for
	// a gateway that answers with an HTML page.
	"openai.error_body_bytes": 1 << 20,
}

func (o OpenAI) validate() error {
	return validateFields("openai", o)
}
