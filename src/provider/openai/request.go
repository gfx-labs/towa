package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"

	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// requestBody is the body of POST /responses and, with Type set, the
// response.create message of the WebSocket transport.
type requestBody struct {
	Type  string `json:"type,omitzero"`
	Model string `json:"model"`
	// PreviousResponseID is set only on a WebSocket whose server still
	// holds that response.
	PreviousResponseID string              `json:"previous_response_id,omitzero"`
	Instructions       string              `json:"instructions"`
	Input              []any               `json:"input"`
	Tools              []any               `json:"tools"`
	ToolChoice         string              `json:"tool_choice"`
	ParallelToolCalls  bool                `json:"parallel_tool_calls"`
	Reasoning          *reasoningParam     `json:"reasoning,omitzero"`
	Store              bool                `json:"store"`
	Stream             bool                `json:"stream"`
	Include            []string            `json:"include"`
	PromptCacheKey     string              `json:"prompt_cache_key,omitzero"`
	MaxOutputTokens    int64               `json:"max_output_tokens,omitzero"`
	ContextManagement  []contextManagement `json:"context_management,omitzero"`
}

type reasoningParam struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type contextManagement struct {
	Type             string `json:"type"`
	CompactThreshold int64  `json:"compact_threshold"`
}

type functionTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Strict      bool           `json:"strict"`
	Parameters  jsontext.Value `json:"parameters"`
}

type messageItem struct {
	Type    string        `json:"type"`
	ID      string        `json:"id,omitzero"`
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
	Phase   string        `json:"phase,omitzero"`
}

type contentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitzero"`
	ImageURL string `json:"image_url,omitzero"`
	Filename string `json:"filename,omitzero"`
	FileData string `json:"file_data,omitzero"`
}

type reasoningItem struct {
	Type             string        `json:"type"`
	ID               string        `json:"id"`
	Summary          []contentPart `json:"summary"`
	EncryptedContent string        `json:"encrypted_content,omitzero"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitzero"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type customCallItem struct {
	Type   string `json:"type"`
	ID     string `json:"id,omitzero"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

type callOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	// Output is a string, or a list of content parts when the result is
	// not one piece of text.
	Output any `json:"output"`
}

// Wire names of the item types this package builds and reads.
const (
	typeMessage          = "message"
	typeReasoning        = "reasoning"
	typeFunctionCall     = "function_call"
	typeCustomCall       = "custom_tool_call"
	typeFunctionOutput   = "function_call_output"
	typeCustomCallOutput = "custom_tool_call_output"
	typeCompaction       = "compaction"
)

// opaque is what Item.Opaque holds for a Text, Thinking or ToolCall item:
// the parts of the wire item that the plain fields do not carry. A Native
// item's Opaque is the wire item itself.
type opaque struct {
	// Type is the wire type of a tool call: a function call whose argument
	// text was not valid JSON is stored as a JSON string, and this is what
	// tells it from a free-text call.
	Type             string `json:"type,omitzero"`
	ID               string `json:"id,omitzero"`
	Phase            string `json:"phase,omitzero"`
	EncryptedContent string `json:"encrypted_content,omitzero"`
	// Summary is the reasoning summary in its parts. Thinking.Text is the
	// parts joined, which cannot be split again.
	Summary []string `json:"summary,omitzero"`
}

func invalidRequest(msg string) *provider.Error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: msg}
}

// buildRequest turns a request into the body for ep.
func (c *Client) buildRequest(ep endpoint, req provider.Request) (*requestBody, *provider.Error) {
	if req.Model == "" {
		return nil, invalidRequest("the request names no model")
	}
	input, perr := buildInput(ep.provider, transcript.Origin{Model: req.Model, Account: req.Credential.Account}, req.Items)
	if perr != nil {
		return nil, perr
	}
	tools, perr := buildTools(req.Tools)
	if perr != nil {
		return nil, perr
	}
	body := &requestBody{
		Model:             req.Model,
		Instructions:      buildInstructions(req.System),
		Input:             input,
		Tools:             tools,
		ToolChoice:        "auto",
		ParallelToolCalls: c.cfg.ParallelToolCalls,
		Store:             false,
		Stream:            true,
		// Without this the reasoning of a stored-nowhere response is lost
		// and the next request starts its reasoning over.
		Include:        []string{"reasoning.encrypted_content"},
		PromptCacheKey: req.CacheKey,
	}
	if req.Effort != "" {
		// A summary is asked for so that a long silent reasoning phase
		// still produces events.
		body.Reasoning = &reasoningParam{Effort: req.Effort, Summary: "auto"}
	}
	// The Codex backend rejects max_output_tokens, so an OAuth request
	// never carries it.
	if !ep.codex {
		body.MaxOutputTokens = req.MaxOutputTokens
	}
	if c.cfg.CompactThreshold > 0 {
		body.ContextManagement = []contextManagement{{Type: typeCompaction, CompactThreshold: c.cfg.CompactThreshold}}
	}
	return body, nil
}

func buildInstructions(system []provider.SystemBlock) string {
	texts := make([]string, 0, len(system))
	for _, b := range system {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func buildTools(tools []provider.Tool) ([]any, *provider.Error) {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if t.NativeAPI != "" && len(t.Native) == 0 {
			return nil, invalidRequest(fmt.Sprintf("tool %q: native API is set without a native definition", t.Name))
		}
		if len(t.Native) > 0 {
			if t.NativeAPI != provider.NativeAPIResponses {
				return nil, invalidRequest(fmt.Sprintf("tool %q: native API %q is not %q", t.Name, t.NativeAPI, provider.NativeAPIResponses))
			}
			if !t.Native.IsValid() {
				return nil, invalidRequest(fmt.Sprintf("tool %q: its native definition is not valid JSON", t.Name))
			}
			out = append(out, t.Native)
			continue
		}
		if t.Name == "" {
			return nil, invalidRequest("a tool has no name")
		}
		if len(t.InputSchema) == 0 || !t.InputSchema.IsValid() {
			return nil, invalidRequest(fmt.Sprintf("tool %q: its input schema is missing or not valid JSON", t.Name))
		}
		// Strict mode needs every property required and no additional
		// properties, which the schemas of MCP tools do not promise.
		out = append(out, functionTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	return out, nil
}

// inputBuilder turns items into wire input items.
type inputBuilder struct {
	own    string
	origin transcript.Origin
	out    []any
	// customCalls holds the ids of free-text calls, whose results go back
	// as a different item type.
	customCalls map[string]bool
}

// buildInput reads opaque metadata only with exact request provenance.
func buildInput(own string, origin transcript.Origin, items []transcript.Item) ([]any, *provider.Error) {
	b := &inputBuilder{own: own, origin: origin, out: make([]any, 0, len(items)), customCalls: map[string]bool{}}
	for i, item := range items {
		if perr := b.add(i, item); perr != nil {
			return nil, perr
		}
	}
	return b.out, nil
}

// add appends the wire item that item makes, when it makes one.
func (b *inputBuilder) add(i int, item transcript.Item) *provider.Error {
	if err := item.Validate(); err != nil {
		return invalidRequest(fmt.Sprintf("item %d: %v", i, err))
	}
	var op opaque
	mine := item.OpaqueCompatible(b.own, b.origin)
	if mine && item.Native == nil {
		if err := json.Unmarshal(item.Opaque, &op); err != nil {
			return invalidRequest(fmt.Sprintf("item %d: its opaque data cannot be read: %v", i, err))
		}
	}
	var perr *provider.Error
	switch {
	case item.User != nil:
		perr = b.addUser(item.User)
	case item.Text != nil:
		b.addText(item.Text, op)
	case item.Thinking != nil:
		b.addThinking(mine, op)
	case item.ToolCall != nil:
		perr = b.addToolCall(item, op, mine)
	case item.ToolResult != nil:
		perr = b.addToolResult(item.ToolResult)
	case item.Native != nil:
		return b.addNative(i, item, mine)
	default:
		return invalidRequest(fmt.Sprintf("item %d: a request cannot hold this kind of item", i))
	}
	if perr != nil {
		return at(perr, i)
	}
	return nil
}

func (b *inputBuilder) addUser(u *transcript.User) *provider.Error {
	role := u.Role
	if role == "" {
		role = "user"
	}
	content, perr := buildContent(u.Parts, false)
	if perr != nil {
		return perr
	}
	if len(content) == 0 {
		return invalidRequest("the user message has nothing to send")
	}
	b.out = append(b.out, messageItem{Type: typeMessage, Role: role, Content: content})
	return nil
}

func (b *inputBuilder) addText(t *transcript.Text, op opaque) {
	// The API rejects an empty text part, and an empty message says
	// nothing.
	if t.Text == "" {
		return
	}
	b.out = append(b.out, messageItem{
		Type: typeMessage, ID: op.ID, Role: "assistant", Phase: op.Phase,
		Content: []contentPart{{Type: "output_text", Text: t.Text}},
	})
}

func (b *inputBuilder) addThinking(mine bool, op opaque) {
	// Reasoning goes back only as the item this endpoint made. The API has
	// no input form for another provider's reasoning.
	if !mine || op.ID == "" {
		return
	}
	summary := make([]contentPart, 0, len(op.Summary))
	for _, text := range op.Summary {
		summary = append(summary, contentPart{Type: "summary_text", Text: text})
	}
	b.out = append(b.out, reasoningItem{Type: typeReasoning, ID: op.ID, Summary: summary, EncryptedContent: op.EncryptedContent})
}

func (b *inputBuilder) addToolCall(item transcript.Item, op opaque, mine bool) *provider.Error {
	// Another endpoint's call goes as a plain function call without its
	// extensions. Text input has no plain form to fall back to.
	if len(item.Opaque) > 0 && !mine && item.ToolCall.Input.Kind() == '"' {
		return invalidRequest("tool call with text input has opaque provenance that does not match provider, model and account")
	}
	call, custom, perr := buildCall(item.ToolCall, op)
	if perr != nil {
		return perr
	}
	b.customCalls[item.ToolCall.ID] = custom
	b.out = append(b.out, call)
	return nil
}

func (b *inputBuilder) addToolResult(tr *transcript.ToolResult) *provider.Error {
	output, perr := buildOutput(tr.Parts)
	if perr != nil {
		return perr
	}
	typ := typeFunctionOutput
	if b.customCalls[tr.CallID] {
		typ = typeCustomCallOutput
	}
	b.out = append(b.out, callOutputItem{Type: typ, CallID: tr.CallID, Output: output})
	return nil
}

func (b *inputBuilder) addNative(i int, item transcript.Item, mine bool) *provider.Error {
	if !mine {
		return invalidRequest(fmt.Sprintf("item %d: native %q requires matching provider, model and account provenance", i, item.Native.Kind))
	}
	if !item.Opaque.IsValid() {
		return invalidRequest(fmt.Sprintf("item %d: the %s item is not valid JSON", i, item.Native.Kind))
	}
	b.out = append(b.out, item.Opaque)
	return nil
}

// at names the item an error is about.
func at(e *provider.Error, i int) *provider.Error {
	e.Message = fmt.Sprintf("item %d: %s", i, e.Message)
	return e
}

// buildCall returns the wire item of a tool call and whether it is a
// free-text call.
func buildCall(call *transcript.ToolCall, op opaque) (any, bool, *provider.Error) {
	if call.ID == "" || call.Name == "" {
		return nil, false, invalidRequest("the tool call has no id or no name")
	}
	if !call.Input.IsValid() {
		return nil, false, invalidRequest(fmt.Sprintf("the input of tool call %q is not valid JSON", call.ID))
	}
	if call.Input.Kind() != '"' {
		if op.Type == typeCustomCall {
			return nil, false, invalidRequest(fmt.Sprintf("custom tool call %q does not have string input", call.ID))
		}
		var compact bytes.Buffer
		enc := jsontext.NewEncoder(&compact)
		if err := enc.WriteValue(call.Input); err != nil {
			return nil, false, invalidRequest(fmt.Sprintf("the input of tool call %q: %v", call.ID, err))
		}
		return functionCallItem{
			Type: typeFunctionCall, ID: op.ID, CallID: call.ID, Name: call.Name,
			Arguments: strings.TrimSuffix(compact.String(), "\n"),
		}, false, nil
	}
	if op.Type != typeCustomCall && op.Type != typeFunctionCall {
		return nil, false, invalidRequest(fmt.Sprintf("tool call %q has string input without this endpoint's call-type provenance", call.ID))
	}
	var text string
	if err := json.Unmarshal(call.Input, &text); err != nil {
		return nil, false, invalidRequest(fmt.Sprintf("the input of tool call %q: %v", call.ID, err))
	}
	// A function call whose argument text was not JSON is stored as a
	// string too. Only this endpoint's own record says which it was.
	if op.Type == typeFunctionCall {
		return functionCallItem{Type: typeFunctionCall, ID: op.ID, CallID: call.ID, Name: call.Name, Arguments: text}, false, nil
	}
	return customCallItem{Type: typeCustomCall, ID: op.ID, CallID: call.ID, Name: call.Name, Input: text}, true, nil
}

// buildOutput returns a tool result's output: a string for one piece of
// text, a list of parts otherwise.
func buildOutput(parts []transcript.Part) (any, *provider.Error) {
	content, perr := buildContent(parts, true)
	if perr != nil {
		return nil, perr
	}
	switch {
	case len(content) == 0:
		return "", nil
	case len(content) == 1 && content[0].Type == "input_text":
		return content[0].Text, nil
	default:
		return content, nil
	}
}

// buildContent turns parts into input content. A Stored part is left out:
// it references content that is never sent to a model. In a tool result, a
// part the API cannot carry becomes text that says so. Anywhere else it is
// an error.
func buildContent(parts []transcript.Part, result bool) ([]contentPart, *provider.Error) {
	out := make([]contentPart, 0, len(parts))
	for _, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			out = append(out, contentPart{Type: "input_text", Text: p.Text})
		case p.Image != nil:
			out = append(out, contentPart{Type: "input_image", ImageURL: dataURL(p.Image.MimeType, p.Image.Data)})
		case p.Audio != nil:
			if !result {
				return nil, invalidRequest("the Responses API takes no audio input")
			}
			out = append(out, contentPart{Type: "input_text", Text: provider.LeftOut(p, apiName)})
		case p.Resource != nil:
			part, perr := buildResource(p.Resource)
			if perr != nil {
				if !result {
					return nil, perr
				}
				part = contentPart{Type: "input_text", Text: provider.LeftOut(p, apiName)}
			}
			out = append(out, part)
		case p.Link != nil:
			out = append(out, contentPart{Type: "input_text", Text: fmt.Sprintf("[%s](%s)", p.Link.Name, p.Link.URI)})
		default:
			return nil, invalidRequest("a content part has nothing set")
		}
	}
	return out, nil
}

// apiName names the API in what a model is told was left out of a result.
const apiName = "the Responses API"

func buildResource(r *transcript.Resource) (contentPart, *provider.Error) {
	switch {
	case r.Text != "" || len(r.Data) == 0:
		// The URI is kept with the text so the model can tell which file
		// it is reading. A resource with no content is an empty file.
		return contentPart{Type: "input_text", Text: fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", r.URI, r.Text)}, nil
	case strings.HasPrefix(r.MimeType, "image/"):
		return contentPart{Type: "input_image", ImageURL: dataURL(r.MimeType, r.Data)}, nil
	case r.MimeType == "application/pdf":
		return contentPart{Type: "input_file", Filename: r.URI, FileData: dataURL(r.MimeType, r.Data)}, nil
	default:
		return contentPart{}, invalidRequest(fmt.Sprintf("resource %q of type %q has no form the Responses API accepts", r.URI, r.MimeType))
	}
}

func dataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}
