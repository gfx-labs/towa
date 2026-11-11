package openai

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"strings"

	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// streamEvent is one event of a response, from either transport.
type streamEvent struct {
	Type         string         `json:"type"`
	Item         jsontext.Value `json:"item"`
	Delta        string         `json:"delta"`
	ItemID       string         `json:"item_id"`
	SummaryIndex int            `json:"summary_index"`
	Response     *responseBody  `json:"response"`

	// An SSE error event carries code and message beside type. A WebSocket
	// error event wraps them in error and adds the HTTP status and headers
	// the same failure would have had.
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Error      jsontext.Value `json:"error"`
	Status     int            `json:"status"`
	StatusCode int            `json:"status_code"`
	Headers    map[string]any `json:"headers"`
}

type responseBody struct {
	ID                string         `json:"id"`
	Status            string         `json:"status"`
	Error             jsontext.Value `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage  *usageBody       `json:"usage"`
	Output []jsontext.Value `json:"output"`
}

type usageBody struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

// outputItem is the part of an output item this package reads. Anything
// else in the item is kept only when the whole item is stored as Native.
type outputItem struct {
	Type             string `json:"type"`
	ID               string `json:"id"`
	Role             string `json:"role"`
	Phase            string `json:"phase"`
	CallID           string `json:"call_id"`
	Name             string `json:"name"`
	Arguments        string `json:"arguments"`
	Input            string `json:"input"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
	Summary []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"summary"`
}

// summarySeparator stands between the parts of a reasoning summary, in the
// deltas and in the finished text alike.
const summarySeparator = "\n\n"

// parser turns the events of one response into provider events.
type parser struct {
	provider string
	model    string
	account  string

	// calls maps the item id of a tool call being written to its call.
	calls map[string]provider.ToolCallProgress
	// thinking says a reasoning delta has been sent, so that the next
	// summary part starts with a separator.
	thinking    bool
	sawToolCall bool
	sawRefusal  bool
	// out holds the finished items yielded, and responseID is the id of the
	// response. The WebSocket transport continues from both.
	out        []transcript.Item
	responseID string
	// stopped says the consumer broke out of the range.
	stopped bool
}

// handle processes one event. It returns true after the event that ends
// the response.
func (p *parser) handle(data []byte, emit func(provider.Event) bool) (bool, *provider.Error) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return false, &provider.Error{
			Class: provider.Retry, Reason: provider.ReasonNetwork,
			Message: "a stream event is not the JSON this client reads", Err: err,
		}
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			emit(provider.Event{TextDelta: ev.Delta})
		}
	case "response.reasoning_summary_part.added":
		if ev.SummaryIndex > 0 && p.thinking {
			emit(provider.Event{ThinkingDelta: summarySeparator})
		}
	case "response.reasoning_summary_text.delta":
		if ev.Delta != "" {
			p.thinking = true
			emit(provider.Event{ThinkingDelta: ev.Delta})
		}
	case "response.output_item.added":
		return false, p.itemAdded(ev.Item, emit)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		// Progress is for display. A delta for a call that was never
		// announced has no call id to report it under, and the finished
		// call still arrives as an item.
		if call, ok := p.calls[ev.ItemID]; ok && ev.Delta != "" {
			call.InputDelta = ev.Delta
			emit(provider.Event{ToolCall: &call})
		}
	case "response.output_item.done":
		return false, p.itemDone(ev.Item, emit)
	case "response.completed":
		return true, p.finish(ev.Response, emit)
	case "response.incomplete":
		return true, p.incomplete(ev.Response, emit)
	case "response.failed":
		var raw jsontext.Value
		if ev.Response != nil {
			raw = ev.Response.Error
		}
		return true, classify(0, nil, parseErrorObject(raw))
	case "error":
		return true, p.errorEvent(ev)
	}
	return false, nil
}

func (p *parser) itemAdded(raw jsontext.Value, emit func(provider.Event) bool) *provider.Error {
	item, perr := decodeItem(raw)
	if perr != nil {
		return perr
	}
	switch item.Type {
	case typeReasoning:
		// Each reasoning item is a separate thought.
		if p.thinking {
			emit(provider.Event{ThinkingDelta: summarySeparator})
		}
	case typeFunctionCall, typeCustomCall:
		if item.ID == "" || item.CallID == "" {
			return nil
		}
		if p.calls == nil {
			p.calls = map[string]provider.ToolCallProgress{}
		}
		call := provider.ToolCallProgress{ID: item.CallID, Name: item.Name}
		p.calls[item.ID] = call
		emit(provider.Event{ToolCall: &call})
	}
	return nil
}

func (p *parser) itemDone(raw jsontext.Value, emit func(provider.Event) bool) *provider.Error {
	item, perr := p.convert(raw)
	if perr != nil || item == nil {
		return perr
	}
	p.out = append(p.out, *item)
	emit(provider.Event{Item: item})
	return nil
}

func decodeItem(raw jsontext.Value) (*outputItem, *provider.Error) {
	var item outputItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, &provider.Error{
			Class: provider.Retry, Reason: provider.ReasonNetwork,
			Message: "an output item is not the JSON this client reads", Err: err,
		}
	}
	return &item, nil
}

// convert turns a finished output item into a transcript item. It returns
// nil for an item with nothing to keep.
func (p *parser) convert(raw jsontext.Value) (*transcript.Item, *provider.Error) {
	item, perr := decodeItem(raw)
	if perr != nil {
		return nil, perr
	}
	out := &transcript.Item{Provider: p.provider}
	op := opaque{ID: item.ID}
	switch item.Type {
	case typeMessage:
		if item.Role != "assistant" {
			return nil, protocolError(fmt.Sprintf("the response holds a message with role %q", item.Role))
		}
		var text strings.Builder
		for _, c := range item.Content {
			switch c.Type {
			case "output_text":
				text.WriteString(c.Text)
			case "refusal":
				p.sawRefusal = true
				text.WriteString(c.Refusal)
			}
		}
		out.Text = &transcript.Text{Text: text.String()}
		op.Phase = item.Phase
	case typeReasoning:
		for _, s := range item.Summary {
			if s.Type == "summary_text" {
				op.Summary = append(op.Summary, s.Text)
			}
		}
		if len(op.Summary) == 0 && item.EncryptedContent == "" {
			return nil, nil
		}
		out.Thinking = &transcript.Thinking{Text: strings.Join(op.Summary, summarySeparator)}
		op.EncryptedContent = item.EncryptedContent
	case typeFunctionCall:
		if item.CallID == "" || item.Name == "" {
			return nil, protocolError("a function call has no call id or no name")
		}
		input := jsontext.Value(item.Arguments)
		// Input is the argument as the model wrote it. Text that is not
		// JSON cannot be stored as a JSON value, so it is kept as a string
		// for the argument repair to read.
		if !input.IsValid() {
			var err error
			if input, err = json.Marshal(item.Arguments); err != nil {
				return nil, protocolError("a function call's arguments cannot be stored: " + err.Error())
			}
		}
		p.sawToolCall = true
		out.ToolCall = &transcript.ToolCall{ID: item.CallID, Name: item.Name, Input: input}
		op.Type = typeFunctionCall
	case typeCustomCall:
		if item.CallID == "" || item.Name == "" {
			return nil, protocolError("a custom tool call has no call id or no name")
		}
		input, err := json.Marshal(item.Input)
		if err != nil {
			return nil, protocolError("a custom tool call's input cannot be stored: " + err.Error())
		}
		p.sawToolCall = true
		out.ToolCall = &transcript.ToolCall{ID: item.CallID, Name: item.Name, Input: input}
		op.Type = typeCustomCall
	case "":
		return nil, protocolError("an output item has no type")
	default:
		// A server-side tool call, a compaction item, or a type newer than
		// this client: kept whole and sent back whole.
		out.Native = &transcript.Native{Kind: item.Type}
		out.Opaque = raw.Clone()
		return out, nil
	}
	data, err := json.Marshal(op)
	if err != nil {
		return nil, protocolError("an output item cannot be stored: " + err.Error())
	}
	out.Opaque = data
	return out, nil
}

// protocolError reports a response this client cannot represent. Sending
// the request again may produce one it can.
func protocolError(msg string) *provider.Error {
	return &provider.Error{Class: provider.Retry, Reason: provider.ReasonNetwork, Message: msg}
}

func (p *parser) finish(resp *responseBody, emit func(provider.Event) bool) *provider.Error {
	if perr := p.usage(resp, emit); perr != nil {
		return perr
	}
	stop := provider.StopEndTurn
	switch {
	case p.sawToolCall:
		stop = provider.StopToolUse
	case p.sawRefusal:
		stop = provider.StopRefusal
	}
	emit(provider.Event{Stop: stop})
	return nil
}

func (p *parser) incomplete(resp *responseBody, emit func(provider.Event) bool) *provider.Error {
	reason := ""
	if resp != nil && resp.IncompleteDetails != nil {
		reason = resp.IncompleteDetails.Reason
	}
	var stop provider.StopReason
	switch reason {
	case "max_output_tokens":
		stop = provider.StopMaxTokens
	case "content_filter":
		stop = provider.StopRefusal
	default:
		return &provider.Error{
			Class: provider.Fail, Reason: provider.ReasonRequest,
			Message: fmt.Sprintf("the response is incomplete for a reason this client does not know: %q", reason),
		}
	}
	if perr := p.usage(resp, emit); perr != nil {
		return perr
	}
	emit(provider.Event{Stop: stop})
	return nil
}

func (p *parser) usage(resp *responseBody, emit func(provider.Event) bool) *provider.Error {
	if resp == nil {
		return nil
	}
	p.responseID = resp.ID
	if resp.Usage == nil {
		return nil
	}
	usage, perr := convertUsage(resp.Usage, p.model, p.account)
	if perr != nil {
		return perr
	}
	emit(provider.Event{Usage: usage})
	return nil
}

func convertUsage(u *usageBody, model, account string) (*transcript.Usage, *provider.Error) {
	read, write := u.InputTokensDetails.CachedTokens, u.InputTokensDetails.CacheWriteTokens
	// input_tokens counts cached tokens too, and the transcript's count
	// leaves them out.
	fresh := u.InputTokens - read - write
	if fresh < 0 {
		return nil, protocolError(fmt.Sprintf("the usage reports %d input tokens, fewer than the %d read from and %d written to the cache",
			u.InputTokens, read, write))
	}
	return &transcript.Usage{
		Model: model, Account: account,
		InputTokens: fresh, OutputTokens: u.OutputTokens,
		CacheReadTokens: read, CacheWriteTokens: write,
		ThoughtTokens: u.OutputTokensDetails.ReasoningTokens,
	}, nil
}

func (p *parser) errorEvent(ev streamEvent) *provider.Error {
	apiErr := parseErrorObject(ev.Error)
	if apiErr.Code == "" {
		apiErr.Code = ev.Code
	}
	if apiErr.Message == "" {
		apiErr.Message = ev.Message
	}
	status := ev.Status
	if status == 0 {
		status = ev.StatusCode
	}
	header := http.Header{}
	for name, value := range ev.Headers {
		switch v := value.(type) {
		case string:
			header.Set(name, v)
		case float64:
			header.Set(name, fmt.Sprintf("%v", v))
		}
	}
	return classify(status, header, apiErr)
}
