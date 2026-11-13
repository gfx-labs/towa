// Package transcript is the conversation format shared by every provider,
// tool and store. A provider client turns items into its wire format and its
// stream back into items. Nothing else in the program knows a wire format.
package transcript

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"time"
	"uuid"
)

// Entry is one stored record of a session's transcript. Entries are appended
// and never rewritten or deleted, so history from before a compaction stays
// readable.
type Entry struct {
	ID uuid.UUID `json:"id"`
	// ParentID is the entry this one follows. It is written and nothing
	// reads it yet: it is there so forking needs no format change.
	ParentID uuid.UUID `json:"parent_id,omitzero"`
	Time     time.Time `json:"time"`
	Item     Item      `json:"item"`
}

// NewEntry returns an entry for item with a new UUIDv7 id.
func NewEntry(parent uuid.UUID, now time.Time, item Item) Entry {
	return Entry{ID: uuid.NewV7(), ParentID: parent, Time: now, Item: item}
}

// Item is one element of a conversation: its Body, and what its provider
// needs to replay it.
type Item struct {
	// Body is what the item is. It holds one of the body types by value.
	Body Body

	// Provider names the provider that produced the item, and Opaque holds
	// what that provider needs to get the item back unchanged: a thinking
	// signature, encrypted reasoning, an item id. Only that provider reads
	// Opaque. Any other provider renders the item from its plain fields, or
	// leaves it out when it has none.
	Provider string
	Opaque   jsontext.Value
	// Origin is nil when the request that produced the item is not known.
	Origin *Origin
}

// Body is what an item is: a User, Text, Thinking, ToolCall, ToolResult,
// Native, Usage, ModelSwitch, Compaction or Interrupted value. No other
// package can add to them.
//
//sumtype:decl
type Body interface {
	body()
}

func (User) body()        {}
func (Text) body()        {}
func (Thinking) body()    {}
func (ToolCall) body()    {}
func (ToolResult) body()  {}
func (Native) body()      {}
func (Usage) body()       {}
func (ModelSwitch) body() {}
func (Compaction) body()  {}
func (Interrupted) body() {}

// stored is an item as it is written: an object with one member named after
// its body, beside the provider's members.
type stored struct {
	User        *User        `json:"user,omitzero"`
	Text        *Text        `json:"text,omitzero"`
	Thinking    *Thinking    `json:"thinking,omitzero"`
	ToolCall    *ToolCall    `json:"tool_call,omitzero"`
	ToolResult  *ToolResult  `json:"tool_result,omitzero"`
	Native      *Native      `json:"native,omitzero"`
	Usage       *Usage       `json:"usage,omitzero"`
	ModelSwitch *ModelSwitch `json:"model_switch,omitzero"`
	Compaction  *Compaction  `json:"compaction,omitzero"`
	Interrupted *Interrupted `json:"interrupted,omitzero"`

	Provider string         `json:"provider,omitzero"`
	Opaque   jsontext.Value `json:"opaque,omitzero"`
	Origin   *Origin        `json:"origin,omitzero"`
}

// MarshalJSONTo writes the item as an object with one member named after its
// body. An item that is not valid is not written.
func (i Item) MarshalJSONTo(enc *jsontext.Encoder) error {
	if err := i.Validate(); err != nil {
		return err
	}
	s := stored{Provider: i.Provider, Opaque: i.Opaque, Origin: i.Origin}
	switch b := i.Body.(type) {
	case User:
		s.User = &b
	case Text:
		s.Text = &b
	case Thinking:
		s.Thinking = &b
	case ToolCall:
		s.ToolCall = &b
	case ToolResult:
		s.ToolResult = &b
	case Native:
		s.Native = &b
	case Usage:
		s.Usage = &b
	case ModelSwitch:
		s.ModelSwitch = &b
	case Compaction:
		s.Compaction = &b
	case Interrupted:
		s.Interrupted = &b
	}
	return json.MarshalEncode(enc, &s)
}

// UnmarshalJSONFrom reads what MarshalJSONTo writes. An object with no body
// member, or with more than one, is an error.
func (i *Item) UnmarshalJSONFrom(dec *jsontext.Decoder) error {
	var s stored
	if err := json.UnmarshalDecode(dec, &s); err != nil {
		return err
	}
	var body Body
	found := 0
	is := func(b Body) {
		body = b
		found++
	}
	if s.User != nil {
		is(*s.User)
	}
	if s.Text != nil {
		is(*s.Text)
	}
	if s.Thinking != nil {
		is(*s.Thinking)
	}
	if s.ToolCall != nil {
		is(*s.ToolCall)
	}
	if s.ToolResult != nil {
		is(*s.ToolResult)
	}
	if s.Native != nil {
		is(*s.Native)
	}
	if s.Usage != nil {
		is(*s.Usage)
	}
	if s.ModelSwitch != nil {
		is(*s.ModelSwitch)
	}
	if s.Compaction != nil {
		is(*s.Compaction)
	}
	if s.Interrupted != nil {
		is(*s.Interrupted)
	}
	if found != 1 {
		return fmt.Errorf("transcript: item has %d bodies", found)
	}
	*i = Item{Body: body, Provider: s.Provider, Opaque: s.Opaque, Origin: s.Origin}
	return i.Validate()
}

// Origin identifies the request that produced opaque content, without credentials.
type Origin struct {
	Model   string `json:"model"`
	Account string `json:"account"`
}

// OpaqueCompatible requires known, identical request provenance for opaque replay.
func (i Item) OpaqueCompatible(provider string, origin Origin) bool {
	return len(i.Opaque) > 0 && provider != "" && i.Provider == provider &&
		i.Origin != nil && origin.Model != "" && origin.Account != "" && *i.Origin == origin
}

// Validate checks that the item has a body, that a declared origin is
// complete and that a user message has a role. A body held by pointer is not
// one of the body types.
func (i Item) Validate() error {
	switch b := i.Body.(type) {
	case nil:
		return errors.New("transcript: item has no body")
	case User:
		if err := b.validate(); err != nil {
			return err
		}
	case Text, Thinking, ToolCall, ToolResult, Native, Usage, ModelSwitch, Compaction, Interrupted:
	default:
		return fmt.Errorf("transcript: %T is not an item body", b)
	}
	if i.Origin != nil && (i.Origin.Model == "" || i.Origin.Account == "") {
		return errors.New("transcript: origin requires a model and an account")
	}
	return nil
}

// User is a user or developer message.
type User struct {
	// Role is user when empty. Developer messages retain their instruction priority.
	Role  string `json:"role,omitzero"`
	Parts []Part `json:"parts"`
}

func (u User) validate() error {
	switch u.Role {
	case "", "user", "developer":
		return nil
	}
	return fmt.Errorf("transcript: unsupported message role %q", u.Role)
}

// Text is text the model wrote to the user.
type Text struct {
	Text string `json:"text"`
}

// Thinking is the model's reasoning. Text may be empty when the provider
// returns reasoning only in a form it alone can read, which is then in the
// item's Opaque.
type Thinking struct {
	Text string `json:"text,omitzero"`
}

// ToolCall is the model asking for a tool to be run.
type ToolCall struct {
	// ID is the provider's id for the call. The result carries it back.
	ID   string `json:"id"`
	Name string `json:"name"`
	// Input is the argument as the model wrote it, before any repair. It is
	// a JSON object for a function tool and a JSON string for a tool that
	// takes free text.
	Input jsontext.Value `json:"input"`
}

// ToolResult answers a ToolCall. Every call gets one, including a call that
// was cancelled.
type ToolResult struct {
	CallID  string  `json:"call_id"`
	Name    string  `json:"name"`
	Parts   []Part  `json:"parts"`
	Outcome Outcome `json:"outcome"`
}

// Outcome says how a tool call ended.
type Outcome string

// Outcomes of a tool call.
const (
	// OutcomeCompleted: the tool ran and its result is in Parts.
	OutcomeCompleted Outcome = "completed"
	// OutcomeFailed: the tool ran and reported an error, which is in Parts.
	OutcomeFailed Outcome = "failed"
	// OutcomeCancelled: the call was cancelled before it had an effect.
	OutcomeCancelled Outcome = "cancelled"
	// OutcomeUnknown: the call was dispatched and nothing says whether it
	// took effect. It is never run again on its own.
	OutcomeUnknown Outcome = "unknown"
)

// Native is an item only its provider understands and that has no plain
// form, such as a server-side tool use or a provider's compaction item. Its
// content is the item's Opaque.
type Native struct {
	// Kind is the provider's name for the item type, for logs and search.
	Kind string `json:"kind"`
}

// Usage is what one model request used. It is recorded so that cost can be
// reported for a session loaded in a new process.
type Usage struct {
	Model   string `json:"model"`
	Account string `json:"account,omitzero"`
	// InputTokens excludes tokens read from or written to the cache.
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitzero"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitzero"`
	// ThoughtTokens is the part of OutputTokens spent on reasoning, when the
	// provider reports it.
	ThoughtTokens int64 `json:"thought_tokens,omitzero"`
	// Cost is nil when the price is not known. It is never estimated.
	Cost *Cost `json:"cost,omitzero"`
}

// Cost is an amount of money in an ISO 4217 currency.
type Cost struct {
	Amount   float64 `json:"amount"`
	Currency string  `json:"currency"`
}

// ModelSwitch records that the session moved to another model.
type ModelSwitch struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Compaction records a summary that stands in for earlier entries. The
// entries stay in the store.
type Compaction struct {
	Summary string `json:"summary"`
	// FirstKept is the first entry after the summarised range.
	FirstKept uuid.UUID `json:"first_kept"`
	// Mode is the compaction mode that produced the summary.
	Mode string `json:"mode"`
}

// Interrupted marks that the turn before it was cancelled. It is rendered
// as the interruption message when the next request is built.
type Interrupted struct{}

// Part is one piece of content in a user message or a tool result. Exactly
// one field is set.
type Part struct {
	Text  string `json:"text,omitzero"`
	Image *Blob  `json:"image,omitzero"`
	Audio *Blob  `json:"audio,omitzero"`
	// Resource is a resource sent with its content.
	Resource *Resource `json:"resource,omitzero"`
	// Link names a resource without its content.
	Link *Link `json:"link,omitzero"`
	// Stored stands for content the store keeps outside the log, such as
	// the full output of a tool whose result was gated. It is never sent to
	// a model: a provider client leaves it out, and the part beside it
	// holds the text the model gets.
	Stored *Stored `json:"stored,omitzero"`
}

// Stored references content kept by the session's store, by content hash.
type Stored struct {
	// Hash is "sha256:" followed by the lowercase hex digest of the content.
	Hash     string `json:"hash"`
	Size     int64  `json:"size"`
	MimeType string `json:"mime_type,omitzero"`
}

// Blob is binary content with its media type.
type Blob struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
}

// Resource is a resource with its content, which is either Text or Data.
type Resource struct {
	URI      string `json:"uri"`
	MimeType string `json:"mime_type,omitzero"`
	Text     string `json:"text,omitzero"`
	Data     []byte `json:"data,omitzero"`
}

// Link is a reference to a resource.
type Link struct {
	URI      string `json:"uri"`
	Name     string `json:"name"`
	MimeType string `json:"mime_type,omitzero"`
}

// Is reports whether the item's body is a B.
func Is[B Body](i Item) bool {
	_, ok := i.Body.(B)
	return ok
}
