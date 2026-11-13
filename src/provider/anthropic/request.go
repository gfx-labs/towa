package anthropic

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/gfx-labs/towa/src/auth"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

type wireRequest struct {
	Model        string         `json:"model"`
	MaxTokens    int64          `json:"max_tokens"`
	System       []*textBlock   `json:"system,omitzero"`
	Messages     []*message     `json:"messages"`
	Tools        []any          `json:"tools,omitzero"`
	Thinking     *thinkingParam `json:"thinking,omitzero"`
	OutputConfig *outputConfig  `json:"output_config,omitzero"`
	Stream       bool           `json:"stream"`
}

type message struct {
	Role string `json:"role"`
	// Content holds pointers to the block types below, and a jsontext.Value
	// for a block that is sent back as it was received.
	Content []any `json:"content"`
}

type cacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitzero"`
}

type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitzero"`
}

// mediaBlock is an image or a document.
type mediaBlock struct {
	Type         string        `json:"type"`
	Source       mediaSource   `json:"source"`
	CacheControl *cacheControl `json:"cache_control,omitzero"`
}

type mediaSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      []byte `json:"data"`
}

type thinkingBlock struct {
	Type string `json:"type"`
	// Thinking is sent even when empty: a block whose text was not shown
	// still has the member.
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
}

type redactedThinkingBlock struct {
	Type string `json:"type"`
	Data string `json:"data"`
}

type toolUseBlock struct {
	Type         string         `json:"type"`
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Input        jsontext.Value `json:"input"`
	CacheControl *cacheControl  `json:"cache_control,omitzero"`
}

type toolResultBlock struct {
	Type         string        `json:"type"`
	ToolUseID    string        `json:"tool_use_id"`
	Content      []any         `json:"content,omitzero"`
	IsError      bool          `json:"is_error,omitzero"`
	CacheControl *cacheControl `json:"cache_control,omitzero"`
}

type toolDef struct {
	Name         string         `json:"name"`
	Description  string         `json:"description,omitzero"`
	InputSchema  jsontext.Value `json:"input_schema"`
	CacheControl *cacheControl  `json:"cache_control,omitzero"`
}

type thinkingParam struct {
	Type         string        `json:"type"`
	BudgetTokens int64         `json:"budget_tokens,omitzero"`
	Display      string        `json:"display,omitzero"`
	BlockBinding *blockBinding `json:"block_binding,omitzero"`
}

type blockBinding struct {
	PrefixMismatchBehavior string `json:"prefix_mismatch_behavior"`
}

type outputConfig struct {
	Effort string `json:"effort"`
}

// thinkingOpaque is the Item.Opaque of a Thinking item from this provider.
// The thinking text is the item's own Text, so it is not stored twice.
type thinkingOpaque struct {
	Signature string `json:"signature,omitzero"`
	// RedactedData is the data of a redacted_thinking block.
	RedactedData string `json:"redacted_data,omitzero"`
}

func invalidRequest(msg string) *provider.Error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: msg}
}

// buildRequest turns a request into the wire body, and returns the thinking
// parameter it used, which the beta header depends on.
func (c *Client) buildRequest(req provider.Request) (*wireRequest, Thinking, *provider.Error) {
	if req.Model == "" {
		return nil, Thinking{}, invalidRequest("the request names no model")
	}
	// The Messages API has no default for max_tokens.
	if req.MaxOutputTokens <= 0 {
		return nil, Thinking{}, invalidRequest(fmt.Sprintf("the Messages API needs max output tokens, and the request has %d", req.MaxOutputTokens))
	}
	out := &wireRequest{Model: req.Model, MaxTokens: req.MaxOutputTokens, Stream: true}

	var thinking Thinking
	if c.cfg.Thinking != nil {
		thinking = c.cfg.Thinking(req.Model)
	}
	switch thinking.Mode {
	case ThinkingOff:
	case ThinkingAdaptive:
		out.Thinking = &thinkingParam{Type: "adaptive", Display: thinking.Display}
		if thinking.DropMismatchedBlocks {
			out.Thinking.BlockBinding = &blockBinding{PrefixMismatchBehavior: "drop_block"}
		}
	case ThinkingManual:
		if thinking.BudgetTokens <= 0 || thinking.BudgetTokens >= req.MaxOutputTokens {
			return nil, Thinking{}, invalidRequest(fmt.Sprintf(
				"the thinking budget of %d tokens is not between 1 and the %d max output tokens", thinking.BudgetTokens, req.MaxOutputTokens))
		}
		out.Thinking = &thinkingParam{Type: "enabled", BudgetTokens: thinking.BudgetTokens}
	default:
		return nil, Thinking{}, invalidRequest(fmt.Sprintf("thinking mode %q is not known", thinking.Mode))
	}
	if req.Effort != "" {
		out.OutputConfig = &outputConfig{Effort: req.Effort}
	}

	if req.Credential.Scheme == auth.SchemeOAuth {
		out.System = append(out.System, &textBlock{Type: "text", Text: billingBlock})
	}
	for _, block := range req.System {
		// The API rejects an empty text block.
		if block.Text == "" {
			continue
		}
		out.System = append(out.System, &textBlock{Type: "text", Text: block.Text})
	}

	var lastTool *toolDef
	for _, tool := range req.Tools {
		if tool.NativeAPI != "" && len(tool.Native) == 0 {
			return nil, Thinking{}, invalidRequest(fmt.Sprintf("tool %q: native API is set without a native definition", tool.Name))
		}
		if len(tool.Native) > 0 {
			if tool.NativeAPI != provider.NativeAPIMessages {
				return nil, Thinking{}, invalidRequest(fmt.Sprintf("tool %q: native API %q is not %q", tool.Name, tool.NativeAPI, provider.NativeAPIMessages))
			}
			out.Tools = append(out.Tools, tool.Native)
			continue
		}
		if tool.Name == "" || len(tool.InputSchema) == 0 {
			return nil, Thinking{}, invalidRequest(fmt.Sprintf("tool %q needs a name and an input schema", tool.Name))
		}
		lastTool = &toolDef{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema}
		out.Tools = append(out.Tools, lastTool)
	}

	messages, perr := c.buildMessages(transcript.Origin{Model: req.Model, Account: req.Credential.Account}, req.Items)
	if perr != nil {
		return nil, Thinking{}, perr
	}
	if len(messages) == 0 {
		return nil, Thinking{}, invalidRequest("the request has no message to send")
	}
	out.Messages = messages

	c.placeBreakpoints(out, lastTool)
	return out, thinking, nil
}

// placeBreakpoints marks the four cache breakpoints the API allows. The
// cached prefix runs tools, system, messages, so each mark covers
// everything before it.
//
//   - the last tool towa defined: a native tool is sent unchanged and
//     so cannot carry the mark;
//   - the last system block. The billing block is never marked;
//   - the end of the last message, which the next request reads;
//   - the end of the user message before it, which is where the request
//     before this one ended. The API looks back only twenty blocks from a
//     mark for an earlier entry, and one turn of tool calls can be longer.
func (c *Client) placeBreakpoints(out *wireRequest, lastTool *toolDef) {
	mark := func() *cacheControl {
		return &cacheControl{Type: "ephemeral", TTL: c.cfg.CacheTTL}
	}
	if lastTool != nil {
		lastTool.CacheControl = mark()
	}
	if n := len(out.System); n > 0 && out.System[n-1].Text != billingBlock {
		out.System[n-1].CacheControl = mark()
	}
	last := len(out.Messages) - 1
	markMessage(out.Messages[last], mark)
	for i := last - 1; i >= 0; i-- {
		if out.Messages[i].Role == "user" {
			markMessage(out.Messages[i], mark)
			break
		}
	}
}

// markMessage marks the last block of m that can carry a breakpoint.
// Thinking blocks and blocks sent back as received cannot.
func markMessage(m *message, mark func() *cacheControl) {
	for i := len(m.Content) - 1; i >= 0; i-- {
		switch block := m.Content[i].(type) {
		case *textBlock:
			block.CacheControl = mark()
		case *mediaBlock:
			block.CacheControl = mark()
		case *toolUseBlock:
			block.CacheControl = mark()
		case *toolResultBlock:
			block.CacheControl = mark()
		default:
			continue
		}
		return
	}
}

// messageBuilder turns items into messages. Neighbouring items of one role
// share a message, because the API wants every tool result in the message
// right after its call.
type messageBuilder struct {
	provider string
	origin   transcript.Origin
	out      []*message
}

// buildMessages turns items into messages.
func (c *Client) buildMessages(origin transcript.Origin, items []transcript.Item) ([]*message, *provider.Error) {
	b := &messageBuilder{provider: c.cfg.Provider, origin: origin}
	for i, item := range items {
		if perr := b.addItem(i, item); perr != nil {
			return nil, perr
		}
	}
	return b.out, nil
}

// add appends blocks to the last message when it has the role, and starts a
// message otherwise.
func (b *messageBuilder) add(role string, blocks ...any) {
	if len(blocks) == 0 {
		return
	}
	if n := len(b.out); n > 0 && b.out[n-1].Role == role {
		b.out[n-1].Content = append(b.out[n-1].Content, blocks...)
		return
	}
	b.out = append(b.out, &message{Role: role, Content: blocks})
}

func (b *messageBuilder) addItem(i int, item transcript.Item) *provider.Error {
	if err := item.Validate(); err != nil {
		return invalidRequest(fmt.Sprintf("item %d: %v", i, err))
	}
	// Opaque replay requires exact request provenance.
	own := item.OpaqueCompatible(b.provider, b.origin)
	switch {
	case item.User != nil:
		return b.addUser(i, item.User)
	case item.Text != nil:
		// The API rejects an empty text block.
		if item.Text.Text != "" {
			b.add("assistant", &textBlock{Type: "text", Text: item.Text.Text})
		}
		return nil
	case item.Thinking != nil:
		// The API takes thinking only with the signature it issued.
		// Thinking from another provider has none and is left out.
		if !own {
			return nil
		}
		return b.addThinking(i, item)
	case item.ToolCall != nil:
		return b.addToolCall(i, item)
	case item.ToolResult != nil:
		return b.addToolResult(item.ToolResult)
	case item.Native != nil:
		// Every native item of this API is a block of the assistant's
		// message, such as a server tool's use and its result.
		if !own {
			return invalidRequest(fmt.Sprintf("item %d: native %q requires matching provider, model and account provenance", i, item.Native.Kind))
		}
		b.add("assistant", item.Opaque)
		return nil
	default:
		return invalidRequest(fmt.Sprintf("item %d is not one a model request can hold", i))
	}
}

func (b *messageBuilder) addUser(i int, u *transcript.User) *provider.Error {
	if u.Role != "" && u.Role != "user" {
		return invalidRequest(fmt.Sprintf("item %d: Messages does not support role %q", i, u.Role))
	}
	blocks, perr := buildContent(u.Parts, false)
	if perr != nil {
		return perr
	}
	b.add("user", blocks...)
	return nil
}

func (b *messageBuilder) addThinking(i int, item transcript.Item) *provider.Error {
	var opaque thinkingOpaque
	if err := json.Unmarshal(item.Opaque, &opaque); err != nil {
		return invalidRequest(fmt.Sprintf("item %d: read thinking signature: %v", i, err))
	}
	switch {
	case opaque.RedactedData != "":
		b.add("assistant", &redactedThinkingBlock{Type: "redacted_thinking", Data: opaque.RedactedData})
	case opaque.Signature != "":
		b.add("assistant", &thinkingBlock{Type: "thinking", Thinking: item.Thinking.Text, Signature: opaque.Signature})
	default:
		return invalidRequest(fmt.Sprintf("item %d: thinking has neither a signature nor redacted data", i))
	}
	return nil
}

func (b *messageBuilder) addToolCall(i int, item transcript.Item) *provider.Error {
	// No call from this API has extensions, so these are another provider's
	// and the call goes without them. Text input has no plain form to fall
	// back to.
	if len(item.Opaque) > 0 && item.ToolCall.Input.Kind() == '"' {
		return invalidRequest(fmt.Sprintf("item %d: tool call with text input carries another provider's opaque data", i))
	}
	input, perr := toolInput(item.ToolCall)
	if perr != nil {
		return perr
	}
	b.add("assistant", &toolUseBlock{Type: "tool_use", ID: item.ToolCall.ID, Name: item.ToolCall.Name, Input: input})
	return nil
}

func (b *messageBuilder) addToolResult(tr *transcript.ToolResult) *provider.Error {
	blocks, perr := buildContent(tr.Parts, true)
	if perr != nil {
		return perr
	}
	b.add("user", &toolResultBlock{
		Type: "tool_result", ToolUseID: tr.CallID, Content: blocks,
		IsError: tr.Outcome != transcript.OutcomeCompleted,
	})
	return nil
}

// toolInput returns the input of a tool_use block, which has to be an
// object. A call whose argument is a JSON string, which is a free-text tool
// of another provider or JSON the model left unfinished, is sent as an
// object with the text under "input", so the model still sees what was
// written.
func toolInput(call *transcript.ToolCall) (jsontext.Value, *provider.Error) {
	switch call.Input.Kind() {
	case '{':
		return call.Input, nil
	case '"':
		wrapped, err := json.Marshal(struct {
			Input jsontext.Value `json:"input"`
		}{call.Input})
		if err != nil {
			return nil, invalidRequest(fmt.Sprintf("tool call %q: %v", call.ID, err))
		}
		return wrapped, nil
	default:
		return nil, invalidRequest(fmt.Sprintf("tool call %q: the input is neither a JSON object nor a JSON string", call.ID))
	}
}

// buildContent turns parts into content blocks. A Stored part is left out:
// it references content that is never sent to a model. The result is nil
// when nothing is left. In a tool result, a part the API cannot carry
// becomes text that says so. Anywhere else it is an error.
func buildContent(parts []transcript.Part, result bool) ([]any, *provider.Error) {
	var out []any
	for _, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			out = append(out, &textBlock{Type: "text", Text: p.Text})
		case p.Image != nil:
			out = append(out, media("image", p.Image.MimeType, p.Image.Data))
		case p.Audio != nil:
			if !result {
				return nil, invalidRequest("the Messages API takes no audio input")
			}
			out = append(out, &textBlock{Type: "text", Text: provider.LeftOut(p, apiName)})
		case p.Resource != nil:
			block, perr := buildResource(p.Resource)
			if perr != nil {
				if !result {
					return nil, perr
				}
				block = &textBlock{Type: "text", Text: provider.LeftOut(p, apiName)}
			}
			out = append(out, block)
		case p.Link != nil:
			out = append(out, &textBlock{Type: "text", Text: fmt.Sprintf("[%s](%s)", p.Link.Name, p.Link.URI)})
		default:
			return nil, invalidRequest("a content part has nothing set")
		}
	}
	return out, nil
}

// apiName names the API in what a model is told was left out of a result.
const apiName = "the Messages API"

func buildResource(r *transcript.Resource) (any, *provider.Error) {
	switch {
	case r.Text != "" || len(r.Data) == 0:
		// The URI is kept with the text so the model can tell which file it
		// is reading. A resource with no content is an empty file.
		return &textBlock{Type: "text", Text: fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", r.URI, r.Text)}, nil
	case strings.HasPrefix(r.MimeType, "image/"):
		return media("image", r.MimeType, r.Data), nil
	case r.MimeType == "application/pdf":
		return media("document", r.MimeType, r.Data), nil
	default:
		return nil, invalidRequest(fmt.Sprintf("resource %q of type %q is not text, an image or a PDF", r.URI, r.MimeType))
	}
}

func media(kind, mimeType string, data []byte) *mediaBlock {
	return &mediaBlock{Type: kind, Source: mediaSource{Type: "base64", MediaType: mimeType, Data: data}}
}
