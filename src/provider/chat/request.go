package chat

import (
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
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
	Messages      *wireMessages    `json:"messages"`
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
	Content    wireContent    `json:"content,omitzero"`
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
	Name string `json:"name"`
	// Arguments is the arguments object as a JSON string, already encoded.
	Arguments jsontext.Value `json:"arguments"`
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

	w.Messages = &wireMessages{
		client: c, origin: transcript.Origin{Model: req.Model, Account: req.Credential.Account},
		system: systemText(req.System), items: req.Items,
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
		if perr, ok := errors.AsType[*provider.Error](err); ok {
			return perr
		}
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: "encode request", Err: err}
	}
	if w.Messages.count == 0 {
		return invalid("request has no system prompt and no items")
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

// wireContent is a message's content: text, which the encoder quotes into
// the body without a copy of its own, or an array of parts already encoded.
// An empty text is still content: a tool result can be empty.
type wireContent struct {
	text  string
	parts jsontext.Value
	set   bool
}

func textContent(s string) wireContent { return wireContent{text: s, set: true} }

func partsContent(v jsontext.Value) wireContent { return wireContent{parts: v, set: true} }

func (c *wireContent) IsZero() bool { return !c.set }

func (c *wireContent) MarshalJSONTo(enc *jsontext.Encoder) error {
	if c.parts != nil {
		return enc.WriteValue(c.parts)
	}
	return enc.WriteToken(jsontext.String(c.text))
}

// wireMessages encodes the conversation's messages as they are built, so
// that a request holds no list of them besides its body.
type wireMessages struct {
	client *Client
	origin transcript.Origin
	system string
	items  []transcript.Item
	// count is how many messages were written.
	count int
}

func (m *wireMessages) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := enc.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}
	opts := enc.Options()
	b := &messageBuilder{client: m.client, origin: m.origin, emit: func(msg *wireMessage) error {
		m.count++
		return json.MarshalEncode(enc, msg, opts)
	}}
	if m.system != "" {
		b.msg = wireMessage{Role: "system", Content: textContent(m.system)}
		if err := b.emit(&b.msg); err != nil {
			return err
		}
	}
	for i, item := range m.items {
		if err := b.add(i, item); err != nil {
			return err
		}
	}
	if err := b.flush(); err != nil {
		return err
	}
	if err := b.attach(len(m.items)); err != nil {
		return err
	}
	return enc.WriteToken(jsontext.EndArray)
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

// messageBuilder turns items into wire messages and emits each when it is
// whole. Thinking, text and tool calls that follow one another are one
// assistant message, as the model wrote them in one response. The message
// and buffers are reused: emit copies a message into the body.
type messageBuilder struct {
	client *Client
	origin transcript.Origin
	emit   func(*wireMessage) error
	// cur is the assistant message being built, when open.
	cur  wireMessage
	open bool
	// args holds the encoded arguments of cur's tool calls.
	args []byte
	// msg is any other message, from its building to its emit.
	msg wireMessage
	// text joins the pieces of one message's text, and quoted holds it
	// encoded.
	text   []byte
	quoted []byte
	// attached holds what the tool results of the current run returned that
	// a tool message cannot carry.
	attached []wirePart
}

// add emits the message or builds the part of one that item makes.
func (b *messageBuilder) add(i int, item transcript.Item) error {
	if err := item.Validate(); err != nil {
		return invalid("items[%d]: %v", i, err)
	}
	if item.ToolResult == nil {
		if err := b.attach(i); err != nil {
			return err
		}
	}
	mine := item.OpaqueCompatible(b.client.cfg.ID, b.origin)
	switch {
	case item.User != nil:
		return b.addUser(i, item.User)
	case item.Text != nil:
		b.assistant()
		b.text = append(b.text, item.Text.Text...)
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

// flush emits the assistant message being built.
func (b *messageBuilder) flush() error {
	if !b.open {
		return nil
	}
	m := &b.cur
	if len(b.text) > 0 {
		content, err := b.quote(b.text)
		if err != nil {
			return err
		}
		m.Content = content
	}
	var err error
	// Reasoning alone is not a message an endpoint accepts.
	if !m.Content.IsZero() || len(m.ToolCalls) > 0 {
		err = b.emit(m)
	}
	b.cur = wireMessage{ToolCalls: m.ToolCalls[:0], ReasoningDetails: m.ReasoningDetails[:0]}
	b.open, b.args, b.text = false, b.args[:0], b.text[:0]
	return err
}

// quote returns text as encoded content, in a buffer the next quote reuses.
func (b *messageBuilder) quote(text []byte) (wireContent, error) {
	quoted, err := jsontext.AppendQuote(b.quoted[:0], text)
	if err != nil {
		return wireContent{}, &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: "message text", Err: err}
	}
	b.quoted = quoted
	return partsContent(quoted), nil
}

// attach writes the content tool results could not carry as one user
// message.
func (b *messageBuilder) attach(i int) error {
	if len(b.attached) == 0 {
		return nil
	}
	content, err := json.Marshal(b.attached)
	if err != nil {
		return &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]", i), Err: err}
	}
	b.attached = nil
	b.msg = wireMessage{Role: "user", Content: partsContent(content)}
	return b.emit(&b.msg)
}

func (b *messageBuilder) assistant() *wireMessage {
	if !b.open {
		b.open = true
		b.cur.Role = "assistant"
	}
	return &b.cur
}

func (b *messageBuilder) addUser(i int, u *transcript.User) error {
	role := u.Role
	if role == "" {
		role = "user"
	}
	if err := b.flush(); err != nil {
		return err
	}
	content, err := b.userContent(i, u.Parts)
	if err != nil {
		return err
	}
	if content.IsZero() {
		return nil
	}
	b.msg = wireMessage{Role: role, Content: content}
	return b.emit(&b.msg)
}

func (b *messageBuilder) addThinking(i int, item transcript.Item) error {
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

func (b *messageBuilder) addToolCall(i int, item transcript.Item, mine bool) error {
	// Another provider's call goes without its extensions. Text input has no
	// plain form to fall back to.
	if len(item.Opaque) > 0 && !mine && item.ToolCall.Input.Kind() == '"' {
		return invalid("items[%d]: tool call with text input has opaque provenance that does not match provider, model and account", i)
	}
	tc := item.ToolCall
	if tc.ID == "" || tc.Name == "" {
		return invalid("items[%d]: tool call has no id or no name", i)
	}
	start := len(b.args)
	args, perr := appendArguments(b.args, i, tc.Input)
	if perr != nil {
		return perr
	}
	// A call before holds a slice of the old array when this one grew it,
	// which stays valid.
	b.args = args
	call := wireToolCall{ID: tc.ID, Type: "function", Function: wireFunction{Name: tc.Name, Arguments: args[start:len(args):len(args)]}}
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

func (b *messageBuilder) addToolResult(i int, tr *transcript.ToolResult) error {
	if err := b.flush(); err != nil {
		return err
	}
	if tr.CallID == "" {
		return invalid("items[%d]: tool result has no call id", i)
	}
	// Most results are one text part, sent as stored without a copy.
	var first string
	var pieces int
	write := func(s string) {
		switch pieces {
		case 0:
			first = s
		case 1:
			b.text = append(b.text[:0], first...)
			fallthrough
		default:
			b.text = append(b.text, s...)
		}
		pieces++
	}
	var binary []wirePart
	var notes []string
	for j, p := range tr.Parts {
		switch {
		case p.Stored != nil:
		case p.Text != "":
			write(p.Text)
		case p.Resource != nil && p.Resource.Data == nil:
			write(resourceText(p.Resource))
		case p.Link != nil:
			write(linkText(p.Link))
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
		if pieces > 0 {
			write("\n\n")
		}
		write(note)
	}
	content := textContent(first)
	if pieces > 1 {
		var err error
		if content, err = b.quote(b.text); err != nil {
			return err
		}
		b.text = b.text[:0]
	}
	b.msg = wireMessage{Role: "tool", ToolCallID: tr.CallID, Content: content}
	return b.emit(&b.msg)
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
func appendArguments(dst []byte, i int, input jsontext.Value) ([]byte, *provider.Error) {
	fail := func(err error) ([]byte, *provider.Error) {
		return nil, &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]: tool call input", i), Err: err}
	}
	quote := func(object []byte) ([]byte, *provider.Error) {
		out, err := jsontext.AppendQuote(dst, object)
		if err != nil {
			return fail(err)
		}
		return out, nil
	}
	if input.Kind() == '{' {
		return quote(input)
	}
	if input.Kind() == '"' {
		var text string
		if err := json.Unmarshal(input, &text); err != nil {
			return fail(err)
		}
		if v := jsontext.Value(text); v.IsValid() && v.Kind() == '{' {
			return quote(v)
		}
	}
	wrapped, err := json.Marshal(struct {
		Input jsontext.Value `json:"input"`
	}{input})
	if err != nil {
		return fail(err)
	}
	return quote(wrapped)
}

func linkText(l *transcript.Link) string {
	if l.Name == "" {
		return l.URI
	}
	return "[" + l.Name + "](" + l.URI + ")"
}

// userContent returns a user message's content: a string when every part is
// text, and an array of parts otherwise. It is zero when no part is sent.
func (b *messageBuilder) userContent(i int, parts []transcript.Part) (wireContent, error) {
	// Most messages are text alone, which needs no list of parts.
	texts, first := 0, ""
	for j, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Image != nil, p.Audio != nil, p.Resource != nil && p.Resource.Data != nil:
			return userParts(i, parts)
		case p.Text != "", p.Resource != nil, p.Link != nil:
			text := partText(p)
			if texts == 1 {
				b.text = append(b.text[:0], first...)
			}
			if texts >= 1 {
				b.text = append(b.text, "\n\n"...)
				b.text = append(b.text, text...)
			}
			if texts == 0 {
				first = text
			}
			texts++
		default:
			return wireContent{}, invalid("items[%d].parts[%d]: part has nothing set", i, j)
		}
	}
	switch texts {
	case 0:
		return wireContent{}, nil
	case 1:
		return textContent(first), nil
	}
	content, err := b.quote(b.text)
	b.text = b.text[:0]
	return content, err
}

// partText is the text a user message sends for a text, a text resource or
// a link.
func partText(p transcript.Part) string {
	switch {
	case p.Text != "":
		return p.Text
	case p.Resource != nil:
		return resourceText(p.Resource)
	default:
		return linkText(p.Link)
	}
}

// userParts is the content of a user message that holds more than text.
func userParts(i int, parts []transcript.Part) (wireContent, error) {
	var wire []wirePart
	for j, p := range parts {
		switch {
		case p.Stored != nil:
		case p.Image != nil, p.Audio != nil, p.Resource != nil && p.Resource.Data != nil:
			part, perr := binaryPart(i, j, p)
			if perr != nil {
				return wireContent{}, perr
			}
			wire = append(wire, part)
		case p.Text != "", p.Resource != nil, p.Link != nil:
			wire = append(wire, wirePart{Type: "text", Text: partText(p)})
		default:
			return wireContent{}, invalid("items[%d].parts[%d]: part has nothing set", i, j)
		}
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return wireContent{}, &provider.Error{Class: provider.Fail, Reason: provider.ReasonRequest, Message: fmt.Sprintf("items[%d]", i), Err: err}
	}
	return partsContent(out), nil
}

// resourceText is a text resource with its URI, so the model can tell
// which file it is reading, as the other two APIs send it.
func resourceText(r *transcript.Resource) string {
	return fmt.Sprintf("<resource uri=%q>\n%s\n</resource>", r.URI, r.Text)
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
