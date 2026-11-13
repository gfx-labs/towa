package anthropic

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// streamEvent is the data of one server-sent event. The type is read from
// the data, which carries it too, and not from the event's name.
type streamEvent struct {
	Type         string         `json:"type"`
	Index        int64          `json:"index"`
	Message      *streamMessage `json:"message"`
	ContentBlock jsontext.Value `json:"content_block"`
	Delta        *streamDelta   `json:"delta"`
	Usage        *streamUsage   `json:"usage"`
	Error        jsontext.Value `json:"error"`
}

type streamMessage struct {
	Usage *streamUsage `json:"usage"`
}

// streamDelta is the delta of a content_block_delta or of a message_delta.
type streamDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// streamUsage has pointers because message_delta repeats only some of the
// counts, and one that is absent keeps its earlier value.
type streamUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             *int64 `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
}

// blockHead is what is read from a content block when it starts.
type blockHead struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
	Text string `json:"text"`
	// Thinking, Signature and Data are set at the start only when the block
	// arrives whole, as redacted thinking does.
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

// block is a content block that is still arriving.
type block struct {
	head blockHead
	// raw is the block as it started, kept for a block that goes back to
	// the API as it came.
	raw       jsontext.Value
	text      strings.Builder
	signature strings.Builder
	// input is the argument text of a tool call.
	input strings.Builder
}

type streamState struct {
	provider string
	origin   transcript.Origin
	usage    transcript.Usage
	blocks   map[int64]*block
	stop     string
}

func newStreamState(providerName string, origin transcript.Origin) *streamState {
	return &streamState{provider: providerName, origin: origin, usage: transcript.Usage{Model: origin.Model}, blocks: map[int64]*block{}}
}

func protocolError(msg string) *provider.Error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: msg}
}

// handle takes the data of one event. It reports done when the stream has
// ended or the consumer stopped listening.
func (s *streamState) handle(data []byte, yield func(provider.Event, error) bool) (done bool, perr *provider.Error) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "read stream event", Err: err}
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			return !s.yieldUsage(ev.Message.Usage, yield), nil
		}
	case "content_block_start":
		return s.startBlock(ev, yield)
	case "content_block_delta":
		return s.deltaBlock(ev, yield)
	case "content_block_stop":
		return s.stopBlock(ev, yield)
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.stop = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			return !s.yieldUsage(ev.Usage, yield), nil
		}
	case "message_stop":
		return s.finish(yield)
	case "error":
		return false, classify(0, nil, parseErrorObject(ev.Error))
	default:
		// ping, and event types added later, which the API documents a
		// client has to tolerate.
	}
	return false, nil
}

func (s *streamState) yieldUsage(u *streamUsage, yield func(provider.Event, error) bool) bool {
	set := func(dst *int64, src *int64) {
		if src != nil {
			*dst = *src
		}
	}
	// input_tokens already leaves out what was read from or written to the
	// cache.
	set(&s.usage.InputTokens, u.InputTokens)
	set(&s.usage.OutputTokens, u.OutputTokens)
	set(&s.usage.CacheReadTokens, u.CacheReadInputTokens)
	set(&s.usage.CacheWriteTokens, u.CacheCreationInputTokens)
	usage := s.usage
	return yield(provider.Event{Usage: &usage}, nil)
}

func (s *streamState) startBlock(ev streamEvent, yield func(provider.Event, error) bool) (bool, *provider.Error) {
	if _, open := s.blocks[ev.Index]; open {
		return false, protocolError(fmt.Sprintf("content block %d started twice", ev.Index))
	}
	b := &block{raw: ev.ContentBlock.Clone()}
	if err := json.Unmarshal(ev.ContentBlock, &b.head); err != nil {
		return false, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "read content block", Err: err}
	}
	s.blocks[ev.Index] = b
	switch b.head.Type {
	case "text":
		b.text.WriteString(b.head.Text)
		if b.head.Text != "" {
			return !yield(provider.Event{TextDelta: b.head.Text}, nil), nil
		}
	case "thinking":
		b.text.WriteString(b.head.Thinking)
		b.signature.WriteString(b.head.Signature)
		if b.head.Thinking != "" {
			return !yield(provider.Event{ThinkingDelta: b.head.Thinking}, nil), nil
		}
	case "tool_use":
		return !yield(provider.Event{ToolCall: &provider.ToolCallProgress{ID: b.head.ID, Name: b.head.Name}}, nil), nil
	}
	return false, nil
}

func (s *streamState) deltaBlock(ev streamEvent, yield func(provider.Event, error) bool) (bool, *provider.Error) {
	b, open := s.blocks[ev.Index]
	if !open || ev.Delta == nil {
		return false, protocolError(fmt.Sprintf("a delta arrived for content block %d, which is not open", ev.Index))
	}
	d := ev.Delta
	switch d.Type {
	case "text_delta":
		b.text.WriteString(d.Text)
		if d.Text != "" {
			return !yield(provider.Event{TextDelta: d.Text}, nil), nil
		}
	case "thinking_delta":
		b.text.WriteString(d.Thinking)
		if d.Thinking != "" {
			return !yield(provider.Event{ThinkingDelta: d.Thinking}, nil), nil
		}
	case "signature_delta":
		b.signature.WriteString(d.Signature)
	case "input_json_delta":
		b.input.WriteString(d.PartialJSON)
		if b.head.Type == "tool_use" && d.PartialJSON != "" {
			return !yield(provider.Event{ToolCall: &provider.ToolCallProgress{
				ID: b.head.ID, Name: b.head.Name, InputDelta: d.PartialJSON,
			}}, nil), nil
		}
	default:
		// citations_delta and delta types added later. Citations are not
		// kept: see the lane note.
	}
	return false, nil
}

func (s *streamState) stopBlock(ev streamEvent, yield func(provider.Event, error) bool) (bool, *provider.Error) {
	b, open := s.blocks[ev.Index]
	if !open {
		return false, protocolError(fmt.Sprintf("content block %d stopped without starting", ev.Index))
	}
	delete(s.blocks, ev.Index)
	item, perr := s.item(b)
	if perr != nil || item == nil {
		return false, perr
	}
	item.Origin = &transcript.Origin{Model: s.origin.Model, Account: s.origin.Account}
	return !yield(provider.Event{Item: item}, nil), nil
}

// item turns a finished block into a transcript item. It returns nil for a
// block that holds nothing.
func (s *streamState) item(b *block) (*transcript.Item, *provider.Error) {
	switch b.head.Type {
	case "text":
		if b.text.Len() == 0 {
			return nil, nil
		}
		return &transcript.Item{Body: transcript.Text{Text: b.text.String()}, Provider: s.provider}, nil
	case "thinking":
		return s.thinkingItem(b.text.String(), thinkingOpaque{Signature: b.signature.String()})
	case "redacted_thinking":
		return s.thinkingItem("", thinkingOpaque{RedactedData: b.head.Data})
	case "tool_use":
		return &transcript.Item{
			Body:     transcript.ToolCall{ID: b.head.ID, Name: b.head.Name, Input: argument(b.input.String())},
			Provider: s.provider,
		}, nil
	default:
		// A server tool's use, its result, and any block type added later:
		// kept whole, and sent back as it came.
		raw, perr := nativeBlock(b)
		if perr != nil {
			return nil, perr
		}
		return &transcript.Item{Body: transcript.Native{Kind: b.head.Type}, Provider: s.provider, Opaque: raw}, nil
	}
}

func (s *streamState) thinkingItem(text string, opaque thinkingOpaque) (*transcript.Item, *provider.Error) {
	item := &transcript.Item{Body: transcript.Thinking{Text: text}, Provider: s.provider}
	// Thinking without a signature cannot go back to the API. It is still
	// an item, for the client to show, and has no Opaque.
	if opaque == (thinkingOpaque{}) {
		return item, nil
	}
	raw, err := json.Marshal(opaque)
	if err != nil {
		return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "encode thinking signature", Err: err}
	}
	item.Opaque = raw
	return item, nil
}

// argument is a tool call's input as the model wrote it. A call with no
// argument text is the empty object. Text that is not a JSON object, which
// is what a response cut off at its token limit leaves, is kept as a JSON
// string so that nothing the model wrote is lost and the item still
// encodes.
func argument(text string) jsontext.Value {
	if strings.TrimSpace(text) == "" {
		return jsontext.Value("{}")
	}
	if v := jsontext.Value(text); v.IsValid() && v.Kind() == '{' {
		return v
	}
	quoted, err := jsontext.AppendQuote(nil, text)
	if err != nil {
		// Only invalid UTF-8 fails, and the stream event was decoded with
		// UTF-8 checked.
		return jsontext.Value(`""`)
	}
	return quoted
}

// nativeBlock returns the block as the API has to get it back. The input of
// a server tool's use arrives as deltas after a start whose input is empty,
// so it is put back into the block.
func nativeBlock(b *block) (jsontext.Value, *provider.Error) {
	if b.input.Len() == 0 {
		return b.raw, nil
	}
	input := jsontext.Value(b.input.String())
	if !input.IsValid() {
		return nil, protocolError(fmt.Sprintf("the input of the %s block is not valid JSON", b.head.Type))
	}
	members := map[string]jsontext.Value{}
	if err := json.Unmarshal(b.raw, &members); err != nil {
		return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "read content block", Err: err}
	}
	members["input"] = input
	raw, err := json.Marshal(members, json.Deterministic(true))
	if err != nil {
		return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonNetwork, Message: "encode content block", Err: err}
	}
	return raw, nil
}

func (s *streamState) finish(yield func(provider.Event, error) bool) (bool, *provider.Error) {
	if len(s.blocks) > 0 {
		return false, protocolError(fmt.Sprintf("the message stopped with %d content blocks still open", len(s.blocks)))
	}
	var stop provider.StopReason
	switch s.stop {
	case "end_turn", "stop_sequence":
		stop = provider.StopEndTurn
	case "tool_use":
		stop = provider.StopToolUse
	case "max_tokens":
		stop = provider.StopMaxTokens
	case "refusal":
		stop = provider.StopRefusal
	case "model_context_window_exceeded":
		return false, &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonContextFull,
			Message: "the response stopped because the model's context window is full",
		}
	case "pause_turn":
		stop = provider.StopPaused
	default:
		return false, &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonRequest,
			Message: fmt.Sprintf("the response stopped for the reason %q, which this client does not know", s.stop),
		}
	}
	yield(provider.Event{Stop: stop}, nil)
	return true, nil
}
