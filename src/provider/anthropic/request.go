package anthropic

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/gfx-labs/acpharness/src/auth"
	"github.com/gfx-labs/acpharness/src/provider"
	"github.com/gfx-labs/acpharness/src/transcript"
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

	messages, perr := c.buildMessages(req.Items)
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
//   - the last tool acpharness defined: a native tool is sent unchanged and
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

// buildMessages turns items into messages. Neighbouring items of one role
// share a message, because the API wants every tool result in the message
// right after its call.
func (c *Client) buildMessages(items []transcript.Item) ([]*message, *provider.Error) {
	var out []*message
	add := func(role string, blocks ...any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, &message{Role: role, Content: blocks})
	}
	for i, item := range items {
		if err := item.Validate(); err != nil {
			return nil, invalidRequest(fmt.Sprintf("item %d: %v", i, err))
		}
		// Opaque is read only on an item this provider produced.
		own := item.Provider == c.cfg.Provider && len(item.Opaque) > 0
		switch {
		case item.User != nil:
			if item.User.Role != "" && item.User.Role != "user" {
				return nil, invalidRequest(fmt.Sprintf("item %d: Messages does not support role %q", i, item.User.Role))
			}
			blocks, perr := buildContent(item.User.Parts)
			if perr != nil {
				return nil, perr
			}
			add("user", blocks...)
		case item.Text != nil:
			// The API rejects an empty text block.
			if item.Text.Text != "" {
				add("assistant", &textBlock{Type: "text", Text: item.Text.Text})
			}
		case item.Thinking != nil:
			// The API takes thinking only with the signature it issued.
			// Thinking from another provider has none and is left out.
			if !own {
				continue
			}
			var opaque thinkingOpaque
			if err := json.Unmarshal(item.Opaque, &opaque); err != nil {
				return nil, invalidRequest(fmt.Sprintf("item %d: read thinking signature: %v", i, err))
			}
			switch {
			case opaque.RedactedData != "":
				add("assistant", &redactedThinkingBlock{Type: "redacted_thinking", Data: opaque.RedactedData})
			case opaque.Signature != "":
				add("assistant", &thinkingBlock{Type: "thinking", Thinking: item.Thinking.Text, Signature: opaque.Signature})
			default:
				return nil, invalidRequest(fmt.Sprintf("item %d: thinking has neither a signature nor redacted data", i))
			}
		case item.ToolCall != nil:
			input, perr := toolInput(item.ToolCall)
			if perr != nil {
				return nil, perr
			}
			add("assistant", &toolUseBlock{Type: "tool_use", ID: item.ToolCall.ID, Name: item.ToolCall.Name, Input: input})
		case item.ToolResult != nil:
			blocks, perr := buildContent(item.ToolResult.Parts)
			if perr != nil {
				return nil, perr
			}
			add("user", &toolResultBlock{
				Type: "tool_result", ToolUseID: item.ToolResult.CallID, Content: blocks,
				IsError: item.ToolResult.Outcome != transcript.OutcomeCompleted,
			})
		case item.Native != nil:
			// Every native item of this API is a block of the assistant's
			// message, such as a server tool's use and its result.
			if own {
				add("assistant", item.Opaque)
			}
		default:
			return nil, invalidRequest(fmt.Sprintf("item %d is not one a model request can hold", i))
		}
	}
	return out, nil
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
// when nothing is left.
func buildContent(parts []transcript.Part) ([]any, *provider.Error) {
	var out []any
	for _, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			out = append(out, &textBlock{Type: "text", Text: p.Text})
		case p.Image != nil:
			out = append(out, media("image", p.Image.MimeType, p.Image.Data))
		case p.Audio != nil:
			return nil, invalidRequest("the Messages API takes no audio input")
		case p.Resource != nil:
			block, perr := buildResource(p.Resource)
			if perr != nil {
				return nil, perr
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

func buildResource(r *transcript.Resource) (any, *provider.Error) {
	switch {
	case r.Text != "":
		// The URI is kept with the text so the model can tell which file it
		// is reading.
		return &textBlock{Type: "text", Text: fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", r.URI, r.Text)}, nil
	case len(r.Data) > 0 && strings.HasPrefix(r.MimeType, "image/"):
		return media("image", r.MimeType, r.Data), nil
	case len(r.Data) > 0 && r.MimeType == "application/pdf":
		return media("document", r.MimeType, r.Data), nil
	default:
		return nil, invalidRequest(fmt.Sprintf("resource %q of type %q has no text and is not an image or a PDF", r.URI, r.MimeType))
	}
}

func media(kind, mimeType string, data []byte) *mediaBlock {
	return &mediaBlock{Type: kind, Source: mediaSource{Type: "base64", MediaType: mimeType, Data: data}}
}
