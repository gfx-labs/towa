package chat

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"io"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// wireRequest is the body of a Chat Completions request.
type wireRequest struct {
	Model         string           `json:"model"`
	Messages      []wireMessage    `json:"messages"`
	Stream        bool             `json:"stream"`
	StreamOptions *streamOptions   `json:"stream_options,omitzero"`
	Tools         []jsontext.Value `json:"tools,omitzero"`
	MaxTokens     int64            `json:"max_tokens,omitzero"`
	// ReasoningEffort is the OpenAI member. Reasoning is OpenRouter's.
	ReasoningEffort string         `json:"reasoning_effort,omitzero"`
	Reasoning       *wireReasoning `json:"reasoning,omitzero"`
	Provider        *wireRouting   `json:"provider,omitzero"`
	PromptCacheKey  string         `json:"prompt_cache_key,omitzero"`
	SessionID       string         `json:"session_id,omitzero"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireReasoning struct {
	Effort  string `json:"effort,omitzero"`
	Exclude bool   `json:"exclude,omitzero"`
}

// wireRouting is OpenRouter's provider object.
type wireRouting struct {
	Order                  []string           `json:"order,omitzero"`
	Only                   []string           `json:"only,omitzero"`
	Ignore                 []string           `json:"ignore,omitzero"`
	AllowFallbacks         *bool              `json:"allow_fallbacks,omitzero"`
	RequireParameters      *bool              `json:"require_parameters,omitzero"`
	DataCollection         string             `json:"data_collection,omitzero"`
	ZDR                    *bool              `json:"zdr,omitzero"`
	Sort                   string             `json:"sort,omitzero"`
	Quantizations          []string           `json:"quantizations,omitzero"`
	MaxPrice               map[string]float64 `json:"max_price,omitzero"`
	PreferredMinThroughput float64            `json:"preferred_min_throughput,omitzero"`
	PreferredMaxLatency    float64            `json:"preferred_max_latency,omitzero"`
}

type wireMessage struct {
	Role string `json:"role"`
	// Content is a JSON string, or an array of parts for a user message
	// that has more than text. It is left out of an assistant message that
	// has only tool calls.
	Content    jsontext.Value `json:"content,omitzero"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitzero"`
	ToolCallID string         `json:"tool_call_id,omitzero"`
	// The three reasoning members are how different endpoints take a
	// model's reasoning back. At most one is set.
	Reasoning        string           `json:"reasoning,omitzero"`
	ReasoningContent string           `json:"reasoning_content,omitzero"`
	ReasoningDetails []jsontext.Value `json:"reasoning_details,omitzero"`
}

type wireToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
	// ExtraContent is returned as it came. Gemini keeps a thought signature
	// in it.
	ExtraContent jsontext.Value `json:"extra_content,omitzero"`
}

type wireFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string      `json:"type"`
	Function wireToolDef `json:"function"`
}

type wireToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitzero"`
	Parameters  jsontext.Value `json:"parameters,omitzero"`
}

type wirePart struct {
	Type       string     `json:"type"`
	Text       string     `json:"text,omitzero"`
	ImageURL   *wireURL   `json:"image_url,omitzero"`
	InputAudio *wireAudio `json:"input_audio,omitzero"`
}

type wireURL struct {
	URL string `json:"url"`
}

type wireAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

// thinkingOpaque is the Opaque of a Thinking item this client produced: how
// the reasoning arrived, so that it goes back the same way.
type thinkingOpaque struct {
	// Field is the delta member the text came in: "reasoning" or
	// "reasoning_content". It is empty when the text came only in Details.
	Field string `json:"field,omitzero"`
	// Details is OpenRouter's reasoning_details, merged from the deltas.
	Details []jsontext.Value `json:"details,omitzero"`
}

// toolCallOpaque is the Opaque of a ToolCall item this client produced.
type toolCallOpaque struct {
	ExtraContent jsontext.Value `json:"extra_content,omitzero"`
}

func invalid(format string, args ...any) *provider.Error {
	return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf(format, args...)}
}

// body encodes the request into dst.
func (c *Client) body(dst io.Writer, req provider.Request) *provider.Error {
	if req.Model == "" {
		return invalid("request has no model")
	}
	w := wireRequest{Model: req.Model, Stream: true, MaxTokens: req.MaxOutputTokens}
	or := c.cfg.OpenRouter
	if or.Enabled {
		if req.Effort != "" || or.Reasoning.Exclude {
			w.Reasoning = &wireReasoning{Effort: req.Effort, Exclude: or.Reasoning.Exclude}
		}
		w.Provider = routing(or.Routing)
	} else {
		w.ReasoningEffort = req.Effort
		// OpenRouter always reports usage and documents this member as
		// having no effect. Other endpoints report usage only when asked.
		w.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	switch c.cfg.CacheKeyField {
	case config.CacheKeyPromptCacheKey:
		w.PromptCacheKey = req.CacheKey
	case config.CacheKeySessionID:
		w.SessionID = req.CacheKey
	}

	if system := systemText(req.System); system != "" {
		w.Messages = append(w.Messages, wireMessage{Role: "system", Content: jsonString(system)})
	}
	msgs, perr := c.messages(transcript.Origin{Model: req.Model, Account: req.Credential.Account}, req.Items)
	if perr != nil {
		return perr
	}
	w.Messages = append(w.Messages, msgs...)
	if len(w.Messages) == 0 {
		return invalid("request has no system prompt and no items")
	}

	for i, t := range req.Tools {
		if t.NativeAPI != "" && len(t.Native) == 0 {
			return invalid("tool %q: native API is set without a native definition", t.Name)
		}
		if t.Native != nil {
			if t.NativeAPI != provider.NativeAPIChatCompletions {
				return invalid("tool %q: native API %q is not %q", t.Name, t.NativeAPI, provider.NativeAPIChatCompletions)
			}
			w.Tools = append(w.Tools, t.Native)
			continue
		}
		if t.Name == "" {
			return invalid("tools[%d] has no name", i)
		}
		def, err := json.Marshal(wireTool{Type: "function", Function: wireToolDef{
			Name: t.Name, Description: t.Description, Parameters: t.InputSchema,
		}})
		if err != nil {
			return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("tool %q", t.Name), Err: err}
		}
		w.Tools = append(w.Tools, def)
	}

	if err := json.MarshalWrite(dst, w); err != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: "encode request", Err: err}
	}
	return nil
}

// routing returns the provider object, or nil when no preference is set.
func routing(r config.OpenRouterRouting) *wireRouting {
	w := wireRouting{
		Order: r.Order, Only: r.Only, Ignore: r.Ignore,
		AllowFallbacks: r.AllowFallbacks, RequireParameters: r.RequireParameters,
		DataCollection: r.DataCollection, ZDR: r.ZDR, Sort: r.Sort,
		Quantizations: r.Quantizations, MaxPrice: r.MaxPrice,
		PreferredMinThroughput: r.PreferredMinThroughputTPS, PreferredMaxLatency: r.PreferredMaxLatencySeconds,
	}
	if len(w.Order)+len(w.Only)+len(w.Ignore)+len(w.Quantizations)+len(w.MaxPrice) == 0 &&
		w.AllowFallbacks == nil && w.RequireParameters == nil && w.ZDR == nil &&
		w.DataCollection == "" && w.Sort == "" && w.PreferredMinThroughput == 0 && w.PreferredMaxLatency == 0 {
		return nil
	}
	return &w
}

// systemText joins the sections into the one system message the protocol
// has. Chat Completions has no cache breakpoints of its own.
func systemText(blocks []provider.SystemBlock) string {
	var texts []string
	for _, b := range blocks {
		if b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

func jsonString(s string) jsontext.Value {
	v, err := jsontext.AppendQuote(nil, s)
	if err != nil {
		// Only invalid UTF-8 fails, and the encoder then reports it for
		// the whole request.
		return jsontext.Value(`"` + s + `"`)
	}
	return v
}

// messageBuilder turns items into wire messages. Thinking, text and tool
// calls that follow one another are one assistant message, as the model
// wrote them in one response.
type messageBuilder struct {
	client *Client
	origin transcript.Origin
	out    []wireMessage
	cur    *wireMessage // the assistant message being built
	text   strings.Builder
	// attached holds what the tool results of the current run returned that
	// a tool message cannot carry.
	attached []wirePart
}

// messages turns the conversation into wire messages.
func (c *Client) messages(origin transcript.Origin, items []transcript.Item) ([]wireMessage, *provider.Error) {
	b := &messageBuilder{client: c, origin: origin}
	for i, item := range items {
		if perr := b.add(i, item); perr != nil {
			return nil, perr
		}
	}
	b.flush()
	if perr := b.attach(len(items)); perr != nil {
		return nil, perr
	}
	return b.out, nil
}

// add appends the message or the part of one that item makes.
func (b *messageBuilder) add(i int, item transcript.Item) *provider.Error {
	if err := item.Validate(); err != nil {
		return invalid("items[%d]: %v", i, err)
	}
	if item.ToolResult == nil {
		if perr := b.attach(i); perr != nil {
			return perr
		}
	}
	mine := item.OpaqueCompatible(b.client.cfg.ID, b.origin)
	switch {
	case item.User != nil:
		return b.addUser(i, item.User)
	case item.Text != nil:
		b.text.WriteString(item.Text.Text)
		b.assistant()
		return nil
	case item.Thinking != nil:
		// Incompatible reasoning has no plain form here.
		if !mine || item.Opaque == nil {
			return nil
		}
		return b.addThinking(i, item)
	case item.ToolCall != nil:
		return b.addToolCall(i, item, mine)
	case item.ToolResult != nil:
		return b.addToolResult(i, item.ToolResult)
	case item.Native != nil:
		return invalid("items[%d]: Chat Completions cannot replay native item of kind %q", i, item.Native.Kind)
	default:
		return invalid("items[%d]: not an item a request can hold", i)
	}
}

// flush ends the assistant message being built.
func (b *messageBuilder) flush() {
	if b.cur == nil {
		return
	}
	if b.text.Len() > 0 {
		b.cur.Content = jsonString(b.text.String())
	}
	// Reasoning alone is not a message an endpoint accepts.
	if b.cur.Content != nil || len(b.cur.ToolCalls) > 0 {
		b.out = append(b.out, *b.cur)
	}
	b.cur = nil
	b.text.Reset()
}

// attach writes the content tool results could not carry as one user
// message.
func (b *messageBuilder) attach(i int) *provider.Error {
	if len(b.attached) == 0 {
		return nil
	}
	content, err := json.Marshal(b.attached)
	if err != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]", i), Err: err}
	}
	b.out = append(b.out, wireMessage{Role: "user", Content: content})
	b.attached = nil
	return nil
}

func (b *messageBuilder) assistant() *wireMessage {
	if b.cur == nil {
		b.cur = &wireMessage{Role: "assistant"}
	}
	return b.cur
}

func (b *messageBuilder) addUser(i int, u *transcript.User) *provider.Error {
	role := u.Role
	if role == "" {
		role = "user"
	}
	b.flush()
	content, perr := userContent(i, u.Parts)
	if perr != nil {
		return perr
	}
	if content != nil {
		b.out = append(b.out, wireMessage{Role: role, Content: content})
	}
	return nil
}

func (b *messageBuilder) addThinking(i int, item transcript.Item) *provider.Error {
	var op thinkingOpaque
	if err := json.Unmarshal(item.Opaque, &op); err != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]: opaque", i), Err: err}
	}
	m := b.assistant()
	switch {
	case len(op.Details) > 0:
		m.ReasoningDetails = append(m.ReasoningDetails, op.Details...)
	case op.Field == fieldReasoningContent:
		m.ReasoningContent += item.Thinking.Text
	case op.Field == fieldReasoning:
		m.Reasoning += item.Thinking.Text
	default:
		return invalid("items[%d]: thinking opaque names the field %q", i, op.Field)
	}
	return nil
}

func (b *messageBuilder) addToolCall(i int, item transcript.Item, mine bool) *provider.Error {
	// Another provider's call goes without its extensions. Text input has no
	// plain form to fall back to.
	if len(item.Opaque) > 0 && !mine && item.ToolCall.Input.Kind() == '"' {
		return invalid("items[%d]: tool call with text input has opaque provenance that does not match provider, model and account", i)
	}
	tc := item.ToolCall
	if tc.ID == "" || tc.Name == "" {
		return invalid("items[%d]: tool call has no id or no name", i)
	}
	args, perr := arguments(i, tc.Input)
	if perr != nil {
		return perr
	}
	call := wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: args}}
	if mine && item.Opaque != nil {
		var op toolCallOpaque
		if err := json.Unmarshal(item.Opaque, &op); err != nil {
			return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]: opaque", i), Err: err}
		}
		call.ExtraContent = op.ExtraContent
	}
	m := b.assistant()
	m.ToolCalls = append(m.ToolCalls, call)
	return nil
}

func (b *messageBuilder) addToolResult(i int, tr *transcript.ToolResult) *provider.Error {
	b.flush()
	if tr.CallID == "" {
		return invalid("items[%d]: tool result has no call id", i)
	}
	var text strings.Builder
	var binary []wirePart
	var notes []string
	for j, p := range tr.Parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			text.WriteString(p.Text)
		case p.Resource != nil && p.Resource.Data == nil:
			text.WriteString(p.Resource.Text)
		case p.Link != nil:
			text.WriteString(linkText(p.Link))
		case p.Image != nil, p.Audio != nil, p.Resource != nil:
			// A part this API has no form for becomes text that says so:
			// the result is stored already, so an error here would fail
			// every later turn of the session.
			if part, perr := binaryPart(i, j, p); perr != nil {
				notes = append(notes, provider.LeftOut(p, "Chat Completions"))
			} else {
				binary = append(binary, part)
			}
		default:
			return invalid("items[%d].parts[%d]: part has nothing set", i, j)
		}
	}
	if len(binary) > 0 {
		// A tool message is text, and an endpoint refuses a user message
		// between the results of one response's calls. The content goes in
		// a user message after the last of them, and each says here and
		// there whose it is.
		notes = append(notes, textAttachedBelow)
		b.attached = append(b.attached, wirePart{Type: "text", Text: fmt.Sprintf(textAttachment, tr.CallID)})
		b.attached = append(b.attached, binary...)
	}
	for _, note := range notes {
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		text.WriteString(note)
	}
	b.out = append(b.out, wireMessage{Role: "tool", ToolCallID: tr.CallID, Content: jsonString(text.String())})
	return nil
}

// What the model reads in place of content a tool message cannot carry, and
// ahead of that content in the user message that carries it.
const (
	textAttachedBelow = "[This result holds content a tool message cannot carry. It is in the user message after the tool results.]"
	textAttachment    = "Content of the result of tool call %s:"
)

// arguments returns the text of a tool call's arguments member, which has to
// hold a JSON object: Ollama and vLLM parse it and refuse the whole request
// otherwise, in every later turn of the session. An object the model wrote
// inside a JSON string, which the argument repair ran, goes back as the
// object. Any other input, which is JSON the model left unfinished or
// another provider's free-text call, is sent as an object with the value
// under "input", so the model still sees what was written.
func arguments(i int, input jsontext.Value) (string, *provider.Error) {
	if input.Kind() == '{' {
		return string(input), nil
	}
	fail := func(err error) (string, *provider.Error) {
		return "", &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]: tool call input", i), Err: err}
	}
	if input.Kind() == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return fail(err)
		}
		if v := jsontext.Value(text); v.IsValid() && v.Kind() == '{' {
			return text, nil
		}
	}
	wrapped, err := json.Marshal(struct {
		Input jsontext.Value `json:"input"`
	}{input})
	if err != nil {
		return fail(err)
	}
	return string(wrapped), nil
}

func linkText(l *transcript.Link) string {
	if l.Name == "" {
		return l.URI
	}
	return "[" + l.Name + "](" + l.URI + ")"
}

// userContent returns a user message's content: a string when every part is
// text, and an array of parts otherwise. It is nil when no part is sent.
func userContent(i int, parts []transcript.Part) (jsontext.Value, *provider.Error) {
	var wire []wirePart
	plain := true
	for j, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			wire = append(wire, wirePart{Type: "text", Text: p.Text})
		case p.Image != nil, p.Audio != nil, p.Resource != nil && p.Resource.Data != nil:
			part, perr := binaryPart(i, j, p)
			if perr != nil {
				return nil, perr
			}
			plain = false
			wire = append(wire, part)
		case p.Resource != nil:
			wire = append(wire, wirePart{Type: "text", Text: p.Resource.Text})
		case p.Link != nil:
			wire = append(wire, wirePart{Type: "text", Text: linkText(p.Link)})
		default:
			return nil, invalid("items[%d].parts[%d]: part has nothing set", i, j)
		}
	}
	if len(wire) == 0 {
		return nil, nil
	}
	if plain {
		texts := make([]string, len(wire))
		for k, p := range wire {
			texts[k] = p.Text
		}
		return jsonString(strings.Join(texts, "\n\n")), nil
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]", i), Err: err}
	}
	return out, nil
}

// binaryPart returns the content part of an image, of audio or of a resource
// that holds an image. A user message is the only one that takes it.
func binaryPart(i, j int, p transcript.Part) (wirePart, *provider.Error) {
	switch {
	case p.Image != nil:
		return imagePart(p.Image.MimeType, p.Image.Data), nil
	case p.Audio != nil:
		format, ok := strings.CutPrefix(p.Audio.MimeType, "audio/")
		if !ok || format == "" {
			return wirePart{}, invalid("items[%d].parts[%d]: %q is not an audio type", i, j, p.Audio.MimeType)
		}
		return wirePart{Type: "input_audio", InputAudio: &wireAudio{
			Data: base64.StdEncoding.EncodeToString(p.Audio.Data), Format: format,
		}}, nil
	case p.Resource != nil && strings.HasPrefix(p.Resource.MimeType, "image/"):
		return imagePart(p.Resource.MimeType, p.Resource.Data), nil
	case p.Resource != nil:
		return wirePart{}, invalid("items[%d].parts[%d]: Chat Completions cannot carry a resource of type %q", i, j, p.Resource.MimeType)
	default:
		return wirePart{}, invalid("items[%d].parts[%d]: part holds no binary content", i, j)
	}
}

func imagePart(mimeType string, data []byte) wirePart {
	return wirePart{Type: "image_url", ImageURL: &wireURL{
		URL: "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data),
	}}
}
