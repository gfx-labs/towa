package config

// OpenAI configures the Responses API client, for both of its endpoints:
// an API key against api.openai.com or a configured base URL, and a ChatGPT
// login against the Codex backend.
type OpenAI struct {
	// Transport is how a request reaches the endpoint: "sse" posts it and
	// reads server-sent events, "websocket" keeps one connection per
	// conversation and sends only the new input on it. sse is the transport
	// with recordings replayed in the test suite. The WebSocket transport
	// has never run against a real server, so it is opt-in.
	Transport string `name:"transport" default:"sse" validate:"oneof=sse websocket" help:"How a request reaches the endpoint: sse posts it and reads server-sent events, websocket keeps one connection per conversation."`
	// MaxIdleSockets is how many WebSocket connections are kept open
	// between requests, one per conversation. It has no default and must be
	// set when Transport is "websocket". It is not read otherwise. Zero is
	// "not set", not a number of sockets: validate refuses it when the
	// transport is websocket. No source gives a number. It depends on how
	// many sessions one process serves.
	MaxIdleSockets int `name:"max_idle_sockets" default:"0" validate:"required_if=Transport websocket,gte=0" help:"WebSocket connections kept open between requests. It must be set when openai.transport is websocket."`
	// ParallelToolCalls lets the model ask for several tools in one
	// response. jcode sends parallel_tool_calls false on every Responses
	// request (crates/jcode-provider-openai-runtime/src/lib.rs,
	// build_response_request). openai/codex ce0323f2 decides it per model.
	ParallelToolCalls bool `name:"parallel_tool_calls" help:"Let the model ask for several tools in one response."`
	// ErrorBodyBytes is how much of an error response is read. A longer
	// one is cut, and the error's message says so. The default is 1 MiB,
	// the same value as openai.DefaultErrorBodyBytes. It is a choice and
	// not a measurement: jcode reads an error body without a bound. The
	// error bodies seen so far are 117 bytes (Ollama's unknown model, in
	// src/provider/openai/testdata) and about 190 bytes (the usage limit
	// body quoted in jcode's openai_rate_limit_format.rs). The bound is for
	// a gateway that answers with an HTML page.
	ErrorBodyBytes int64 `name:"error_body_bytes" default:"1048576" validate:"gt=0" help:"How much of an error response is read. A longer one is cut, and the error's message says so."`
}

// Transports of the Responses API client.
const (
	OpenAITransportSSE       = "sse"
	OpenAITransportWebSocket = "websocket"
)

func (o OpenAI) validate() error {
	return validateFields("openai", o)
}
