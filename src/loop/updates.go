package loop

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Mapper turns transcript items into the session updates that show them to
// a client. A running turn and a replay of stored history use the same one,
// so a loaded session looks as it did live.
type Mapper struct {
	// Tools describes tool calls: title, kind and locations. A call to a
	// tool the set does not have is shown under the tool's name with the
	// kind "other".
	Tools tools.Set
	// ContextSize returns the context window of a model in tokens. A usage
	// update needs it, so none is made when it is nil or reports false.
	ContextSize func(model string) (int, bool)
}

// Updates returns the updates for one item, in order. Items a client is not
// shown give none: Native, ModelSwitch, Compaction and Interrupted.
func (m Mapper) Updates(item transcript.Item) ([]acp.SessionUpdate, error) {
	if err := item.Validate(); err != nil {
		return nil, err
	}
	switch {
	case item.User != nil:
		var out []acp.SessionUpdate
		for _, part := range item.User.Parts {
			block, ok, err := Block(part)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, acp.SessionUpdate{UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: block}})
			}
		}
		return out, nil
	case item.Text != nil:
		return []acp.SessionUpdate{TextChunk(item.Text.Text)}, nil
	case item.Thinking != nil:
		// Reasoning only its provider can read has no text to show.
		if item.Thinking.Text == "" {
			return nil, nil
		}
		return []acp.SessionUpdate{ThoughtChunk(item.Thinking.Text)}, nil
	case item.ToolCall != nil:
		call := m.ToolCall(*item.ToolCall)
		return []acp.SessionUpdate{{ToolCall: &call}}, nil
	case item.ToolResult != nil:
		update, err := ToolResult(*item.ToolResult, nil)
		if err != nil {
			return nil, err
		}
		return []acp.SessionUpdate{{ToolCallUpdate: &update}}, nil
	case item.Usage != nil:
		if update, ok := m.Usage(*item.Usage); ok {
			return []acp.SessionUpdate{{UsageUpdate: &update}}, nil
		}
	}
	return nil, nil
}

// TextChunk is text the model wrote to the user.
func TextChunk(text string) acp.SessionUpdate {
	return acp.SessionUpdate{AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: textBlock(text)}}
}

// ThoughtChunk is a piece of the model's reasoning.
func ThoughtChunk(text string) acp.SessionUpdate {
	return acp.SessionUpdate{AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: textBlock(text)}}
}

func textBlock(text string) acp.ContentBlock {
	return acp.ContentBlock{Text: &acp.ContentBlockText{Type: "text", Text: text}}
}

// ToolCall is the update that announces a call. Its status is pending: the
// call has not been allowed or started.
func (m Mapper) ToolCall(call transcript.ToolCall) acp.SessionUpdateToolCall {
	out := acp.SessionUpdateToolCall{
		ToolCallId: acp.ToolCallId(call.ID),
		Name:       &call.Name,
		Title:      call.Name,
		Kind:       acp.ToolKindOther,
		Status:     acp.ToolCallStatusPending,
	}
	if len(call.Input) != 0 {
		out.RawInput = call.Input
	}
	tool, ok := m.Tools.Lookup(call.Name)
	if !ok {
		return out
	}
	// An argument the tool cannot describe is shown under the tool's name.
	// Running the call reports what is wrong with it.
	if d, err := tool.Describe(call.Input); err == nil {
		out.Title, out.Kind, out.Locations = d.Title, d.Kind, d.Locations
	}
	return out
}

// Status is the tool call status a client sees for an outcome. ACP has no
// status for a cancelled call or one with an unknown outcome: both are
// failed, and the result's text says which.
func Status(outcome transcript.Outcome) (acp.ToolCallStatus, error) {
	switch outcome {
	case transcript.OutcomeCompleted:
		return acp.ToolCallStatusCompleted, nil
	case transcript.OutcomeFailed, transcript.OutcomeCancelled, transcript.OutcomeUnknown:
		return acp.ToolCallStatusFailed, nil
	}
	return "", fmt.Errorf("loop: tool result has the outcome %q, which is not one of the four", outcome)
}

// ToolResult is the update that ends a call. content is what the tool gave
// for the client, such as a diff. When it is empty, as it is for a replay,
// the client is shown what the model got.
func ToolResult(result transcript.ToolResult, content []acp.ToolCallContent) (acp.SessionToolCallUpdate, error) {
	status, err := Status(result.Outcome)
	if err != nil {
		return acp.SessionToolCallUpdate{}, err
	}
	if len(content) == 0 {
		for _, part := range result.Parts {
			block, ok, err := Block(part)
			if err != nil {
				return acp.SessionToolCallUpdate{}, err
			}
			if ok {
				content = append(content, acp.ToolCallContent{Content: &acp.ToolCallContentContent{Type: "content", Content: block}})
			}
		}
	}
	return acp.SessionToolCallUpdate{ToolCallId: acp.ToolCallId(result.CallID), Status: &status, Content: content}, nil
}

// Usage is the update for one model request's usage: how much of the
// model's context the request filled, and its cost when the price is known.
func (m Mapper) Usage(u transcript.Usage) (acp.SessionUsageUpdate, bool) {
	if m.ContextSize == nil {
		return acp.SessionUsageUpdate{}, false
	}
	size, ok := m.ContextSize(u.Model)
	if !ok {
		return acp.SessionUsageUpdate{}, false
	}
	out := acp.SessionUsageUpdate{
		Size: size,
		// What the request read and what it wrote are both in the context
		// of the next request.
		Used: int(u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens + u.OutputTokens),
	}
	if u.Cost != nil {
		out.Cost = &acp.Cost{Amount: u.Cost.Amount, Currency: u.Cost.Currency}
	}
	return out, true
}

// Block returns a part as ACP content. It reports false for a part a client
// is not shown: a reference to stored content.
func Block(part transcript.Part) (acp.ContentBlock, bool, error) {
	switch {
	case part.Stored != nil:
		return acp.ContentBlock{}, false, nil
	case part.Image != nil:
		return acp.ContentBlock{Image: &acp.ContentBlockImage{
			Type: "image", MimeType: part.Image.MimeType, Data: base64.StdEncoding.EncodeToString(part.Image.Data),
		}}, true, nil
	case part.Audio != nil:
		return acp.ContentBlock{Audio: &acp.ContentBlockAudio{
			Type: "audio", MimeType: part.Audio.MimeType, Data: base64.StdEncoding.EncodeToString(part.Audio.Data),
		}}, true, nil
	case part.Resource != nil:
		r := part.Resource
		var mime *string
		if r.MimeType != "" {
			mime = &r.MimeType
		}
		res := acp.EmbeddedResourceResource{TextResourceContents: &acp.TextResourceContents{Uri: r.URI, MimeType: mime, Text: r.Text}}
		if r.Data != nil {
			res = acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{
				Uri: r.URI, MimeType: mime, Blob: base64.StdEncoding.EncodeToString(r.Data),
			}}
		}
		return acp.ContentBlock{Resource: &acp.ContentBlockResource{Type: "resource", Resource: res}}, true, nil
	case part.Link != nil:
		l := part.Link
		block := &acp.ContentBlockResourceLink{Type: "resource_link", Uri: l.URI, Name: l.Name}
		if l.MimeType != "" {
			block.MimeType = &l.MimeType
		}
		return acp.ContentBlock{ResourceLink: block}, true, nil
	default:
		// A part with nothing else set is text, which may be empty.
		return textBlock(part.Text), true, nil
	}
}

// Part returns ACP content as a transcript part. It is the reverse of
// Block.
func Part(block acp.ContentBlock) (transcript.Part, error) {
	switch {
	case block.Text != nil:
		return transcript.Part{Text: block.Text.Text}, nil
	case block.Image != nil:
		data, err := base64.StdEncoding.DecodeString(block.Image.Data)
		if err != nil {
			return transcript.Part{}, fmt.Errorf("loop: image content is not base64: %w", err)
		}
		return transcript.Part{Image: &transcript.Blob{MimeType: block.Image.MimeType, Data: data}}, nil
	case block.Audio != nil:
		data, err := base64.StdEncoding.DecodeString(block.Audio.Data)
		if err != nil {
			return transcript.Part{}, fmt.Errorf("loop: audio content is not base64: %w", err)
		}
		return transcript.Part{Audio: &transcript.Blob{MimeType: block.Audio.MimeType, Data: data}}, nil
	case block.ResourceLink != nil:
		l := block.ResourceLink
		link := &transcript.Link{URI: l.Uri, Name: l.Name}
		if l.MimeType != nil {
			link.MimeType = *l.MimeType
		}
		return transcript.Part{Link: link}, nil
	case block.Resource != nil:
		switch r := block.Resource.Resource; {
		case r.TextResourceContents != nil:
			res := &transcript.Resource{URI: r.TextResourceContents.Uri, Text: r.TextResourceContents.Text}
			if r.TextResourceContents.MimeType != nil {
				res.MimeType = *r.TextResourceContents.MimeType
			}
			return transcript.Part{Resource: res}, nil
		case r.BlobResourceContents != nil:
			data, err := base64.StdEncoding.DecodeString(r.BlobResourceContents.Blob)
			if err != nil {
				return transcript.Part{}, fmt.Errorf("loop: resource content is not base64: %w", err)
			}
			res := &transcript.Resource{URI: r.BlobResourceContents.Uri, Data: data}
			if r.BlobResourceContents.MimeType != nil {
				res.MimeType = *r.BlobResourceContents.MimeType
			}
			return transcript.Part{Resource: res}, nil
		}
	}
	return transcript.Part{}, errors.New("loop: content block has no variant set")
}

// UserItem returns a prompt as the transcript's user item.
func UserItem(prompt []acp.ContentBlock) (transcript.Item, error) {
	parts := make([]transcript.Part, 0, len(prompt))
	for _, block := range prompt {
		part, err := Part(block)
		if err != nil {
			return transcript.Item{}, err
		}
		parts = append(parts, part)
	}
	return transcript.Item{User: &transcript.User{Parts: parts}}, nil
}
