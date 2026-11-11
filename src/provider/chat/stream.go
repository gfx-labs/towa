package chat

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// The delta members reasoning text arrives in. OpenRouter uses the first,
// DeepSeek, z.ai and Moonshot the second.
const (
	fieldReasoning        = "reasoning"
	fieldReasoningContent = "reasoning_content"
)

// wireChunk is one data event of a response stream.
type wireChunk struct {
	Model   string         `json:"model"`
	Choices []wireChoice   `json:"choices"`
	Usage   *wireUsage     `json:"usage"`
	Error   jsontext.Value `json:"error"`
}

type wireChoice struct {
	Delta        wireDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}

type wireDelta struct {
	Content          string                      `json:"content"`
	Refusal          string                      `json:"refusal"`
	Reasoning        string                      `json:"reasoning"`
	ReasoningContent string                      `json:"reasoning_content"`
	ReasoningDetails []map[string]jsontext.Value `json:"reasoning_details"`
	ToolCalls        []wireToolCallDelta         `json:"tool_calls"`
}

type wireToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name string `json:"name"`
		// Arguments is a string fragment. Some endpoints send a whole
		// JSON value in its place.
		Arguments jsontext.Value `json:"arguments"`
	} `json:"function"`
	ExtraContent jsontext.Value `json:"extra_content"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	// Cost is what OpenRouter charged for the request, in USD.
	Cost                *float64 `json:"cost"`
	PromptTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
	// PromptCacheHitTokens is DeepSeek's name for cached tokens.
	PromptCacheHitTokens int64 `json:"prompt_cache_hit_tokens"`
}

// toolCall is a tool call being assembled from its deltas.
type toolCall struct {
	id, name string
	args     strings.Builder
	extra    jsontext.Value
}

// streamState assembles one response.
type streamState struct {
	client *Client
	req    provider.Request
	status int
	header http.Header
	// out holds the events of the chunk being read.
	out []provider.Event

	text           strings.Builder
	thinking       strings.Builder
	thinkingField  string
	details        []map[string]jsontext.Value
	detailsByIndex map[string]int
	calls          []*toolCall
	callsByIndex   map[int]*toolCall
	refused        bool
	finish         string
}

// malformed is a response the client cannot read. The request itself was
// accepted, so it is sent again.
func (s *streamState) malformed(err error, format string, args ...any) *provider.Error {
	return &provider.Error{
		Class: provider.Retry, Reason: provider.ReasonNetwork, Status: s.status,
		Message: "malformed response: " + fmt.Sprintf(format, args...), Err: err,
	}
}

// chunk reads one data event.
func (s *streamState) chunk(data []byte) *provider.Error {
	var c wireChunk
	if err := json.Unmarshal(data, &c); err != nil {
		return s.malformed(err, "a stream event is not a chat completion chunk")
	}
	if c.Error != nil && c.Error.Kind() != 'n' {
		// A provider that fails after it sent the status reports the
		// error in the stream.
		return s.client.classifyError(s.status, s.header, decodeError(c.Error))
	}
	for _, choice := range c.Choices {
		d := choice.Delta
		switch {
		case d.Reasoning != "":
			s.think(fieldReasoning, d.Reasoning)
		case d.ReasoningContent != "":
			s.think(fieldReasoningContent, d.ReasoningContent)
		}
		for _, detail := range d.ReasoningDetails {
			// The text of a detail repeats the reasoning member when
			// both are sent.
			if d.Reasoning == "" && d.ReasoningContent == "" {
				if text := stringMember(detail, "text"); text != "" {
					s.think("", text)
				}
			}
			s.mergeDetail(detail)
		}
		if d.Content != "" {
			s.text.WriteString(d.Content)
			s.out = append(s.out, provider.Event{TextDelta: d.Content})
		}
		if d.Refusal != "" {
			s.refused = true
			s.text.WriteString(d.Refusal)
			s.out = append(s.out, provider.Event{TextDelta: d.Refusal})
		}
		for _, tc := range d.ToolCalls {
			if perr := s.toolCallDelta(tc); perr != nil {
				return perr
			}
		}
		if choice.FinishReason != "" {
			s.finish = choice.FinishReason
		}
	}
	if c.Usage != nil {
		u, perr := s.usage(c.Usage)
		if perr != nil {
			return perr
		}
		s.out = append(s.out, provider.Event{Usage: u})
	}
	return nil
}

func (s *streamState) think(field, text string) {
	if field != "" {
		s.thinkingField = field
	}
	s.thinking.WriteString(text)
	s.out = append(s.out, provider.Event{ThinkingDelta: text})
}

func stringMember(obj map[string]jsontext.Value, name string) string {
	v, ok := obj[name]
	if !ok || v.Kind() != '"' {
		return ""
	}
	var out string
	if json.Unmarshal(v, &out) != nil {
		return ""
	}
	return out
}

// detailText are the members of a reasoning detail that arrive in pieces.
var detailText = []string{"text", "summary", "data"}

// mergeDetail adds a streamed piece of a reasoning detail to the detail
// with the same index. OpenRouter wants the merged details back unchanged.
func (s *streamState) mergeDetail(piece map[string]jsontext.Value) {
	index, hasIndex := piece["index"]
	at, known := s.detailsByIndex[string(index)]
	if !hasIndex || !known {
		if hasIndex {
			if s.detailsByIndex == nil {
				s.detailsByIndex = map[string]int{}
			}
			s.detailsByIndex[string(index)] = len(s.details)
		}
		s.details = append(s.details, piece)
		return
	}
	have := s.details[at]
	for name, value := range piece {
		old, exists := have[name]
		switch {
		case value.Kind() == 'n' && exists:
			// A null in a later piece does not erase what came before.
		case exists && old.Kind() == '"' && value.Kind() == '"' && isDetailText(name):
			have[name] = jsonString(stringMember(have, name) + stringMember(piece, name))
		default:
			have[name] = value
		}
	}
}

func isDetailText(name string) bool {
	for _, n := range detailText {
		if n == name {
			return true
		}
	}
	return false
}

func (s *streamState) toolCallDelta(d wireToolCallDelta) *provider.Error {
	call, ok := s.callsByIndex[d.Index]
	if !ok {
		call = &toolCall{}
		if s.callsByIndex == nil {
			s.callsByIndex = map[int]*toolCall{}
		}
		s.callsByIndex[d.Index] = call
		s.calls = append(s.calls, call)
	}
	started := false
	if d.ID != "" && call.id == "" {
		call.id = d.ID
		started = true
	}
	if d.Function.Name != "" && call.name == "" {
		call.name = d.Function.Name
		started = true
	}
	if d.ExtraContent != nil && d.ExtraContent.Kind() != 'n' {
		call.extra = d.ExtraContent
	}
	var fragment string
	switch d.Function.Arguments.Kind() {
	case 0, 'n':
	case '"':
		if err := json.Unmarshal(d.Function.Arguments, &fragment); err != nil {
			return s.malformed(err, "tool call arguments")
		}
	default:
		fragment = string(d.Function.Arguments)
	}
	call.args.WriteString(fragment)
	if started || fragment != "" {
		s.out = append(s.out, provider.Event{ToolCall: &provider.ToolCallProgress{
			ID: call.id, Name: call.name, InputDelta: fragment,
		}})
	}
	return nil
}

func (s *streamState) usage(u *wireUsage) (*transcript.Usage, *provider.Error) {
	cached := u.PromptTokensDetails.CachedTokens
	if cached == 0 {
		cached = u.PromptCacheHitTokens
	}
	written := u.PromptTokensDetails.CacheWriteTokens
	// prompt_tokens counts cached tokens too. Usage.InputTokens does not.
	input := u.PromptTokens - cached - written
	if input < 0 {
		return nil, s.malformed(nil, "usage has %d prompt tokens, fewer than its %d cached and %d cache write tokens",
			u.PromptTokens, cached, written)
	}
	out := &transcript.Usage{
		Model:            s.req.Model,
		Account:          s.req.Credential.Account,
		InputTokens:      input,
		OutputTokens:     u.CompletionTokens,
		CacheReadTokens:  cached,
		CacheWriteTokens: written,
		ThoughtTokens:    u.CompletionTokensDetails.ReasoningTokens,
	}
	if s.client.cfg.OpenRouter.Enabled && u.Cost != nil {
		out.Cost = &transcript.Cost{Amount: *u.Cost, Currency: "USD"}
	}
	return out, nil
}

// end turns what was assembled into items and the stop event.
func (s *streamState) end() *provider.Error {
	id := s.client.cfg.ID
	var stop provider.StopReason
	switch s.finish {
	case "stop", "":
		stop = provider.StopEndTurn
	case "tool_calls", "function_call":
		stop = provider.StopToolUse
	case "length":
		stop = provider.StopMaxTokens
	case "content_filter", "sensitive": // "sensitive" is z.ai's
		stop = provider.StopRefusal
	case "error", "network_error": // "network_error" is z.ai's
		return &provider.Error{
			Class: provider.Retry, Reason: provider.ReasonOverload, Status: s.status,
			Message: fmt.Sprintf("the response ended with finish reason %q and no error", s.finish),
		}
	default:
		return s.malformed(nil, "unknown finish reason %q", s.finish)
	}
	if s.refused {
		stop = provider.StopRefusal
	}
	// Some endpoints finish with "stop" after writing tool calls.
	if len(s.calls) > 0 && stop == provider.StopEndTurn {
		stop = provider.StopToolUse
	}

	if s.thinking.Len() > 0 || len(s.details) > 0 {
		opaque, err := json.Marshal(thinkingOpaque{Field: s.thinkingField, Details: s.detailValues()}, json.Deterministic(true))
		if err != nil {
			return s.malformed(err, "reasoning details")
		}
		s.out = append(s.out, provider.Event{Item: &transcript.Item{
			Thinking: &transcript.Thinking{Text: s.thinking.String()}, Provider: id, Opaque: opaque,
		}})
	}
	if s.text.Len() > 0 {
		s.out = append(s.out, provider.Event{Item: &transcript.Item{
			Text: &transcript.Text{Text: s.text.String()}, Provider: id,
		}})
	}
	for i, call := range s.calls {
		if call.id == "" || call.name == "" {
			return s.malformed(nil, "tool call %d has no id or no name", i)
		}
		args := call.args.String()
		input := jsontext.Value(args)
		if !input.IsValid() {
			// Kept as the model wrote it, in a JSON string, for the
			// tool's argument repair to judge. It goes back unquoted.
			input = jsonString(args)
		}
		item := &transcript.Item{
			ToolCall: &transcript.ToolCall{ID: call.id, Name: call.name, Input: input}, Provider: id,
		}
		if call.extra != nil {
			opaque, err := json.Marshal(toolCallOpaque{ExtraContent: call.extra})
			if err != nil {
				return s.malformed(err, "tool call %d", i)
			}
			item.Opaque = opaque
		}
		s.out = append(s.out, provider.Event{Item: item})
	}
	s.out = append(s.out, provider.Event{Stop: stop})
	return nil
}

func (s *streamState) detailValues() []jsontext.Value {
	out := make([]jsontext.Value, 0, len(s.details))
	for _, d := range s.details {
		v, err := json.Marshal(d, json.Deterministic(true))
		if err != nil {
			continue // members are values the decoder already accepted
		}
		out = append(out, v)
	}
	return out
}
