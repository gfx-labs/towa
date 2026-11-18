// Package hostedimage is image generation run by the model's provider: the
// tool definition the Responses API takes, and a reading of the item a
// generation leaves in the transcript. That item holds the image and goes
// back only to the provider, model and account that made it, so this package
// also gives the text that stands for one in a request to another.
package hostedimage

import (
	"encoding/base64"
	"encoding/json/v2"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Name is the provider's name for the tool.
const Name = "image_generation"

// Kind is the type of the output item a generation leaves, as
// transcript.Native.Kind.
const Kind = "image_generation_call"

// Tool returns the hosted image generation tool for a chain entry, and false
// when the entry has none. Only the built-in codex provider has it: a
// configured endpoint that speaks the same API may not implement it, and
// the Messages API has no such tool. As Codex does, it is offered only to a
// model the account's model list says takes image input, so a model the
// list does not describe goes without it. A model with codex in its name
// refuses hosted tools it does not know, as it does web search.
func Tool(cfg config.ImageGeneration, providerID, model string, inputModalities []string) (provider.Tool, bool, error) {
	if !cfg.Enabled || providerID != config.ProviderCodex || strings.Contains(strings.ToLower(model), "codex") || !slices.Contains(inputModalities, "image") {
		return provider.Tool{}, false, nil
	}
	raw, err := json.Marshal(map[string]any{"type": Name, "output_format": cfg.OutputFormat}, json.Deterministic(true))
	if err != nil {
		return provider.Tool{}, false, fmt.Errorf("hostedimage: %w", err)
	}
	return provider.Tool{Name: Name, Native: raw, NativeAPI: provider.NativeAPIResponses}, true, nil
}

// Unavailable is the rule for a prompt whose request is sent without the
// tool, so that a model asked for an image says it cannot make one here
// instead of answering as if it had.
const Unavailable = Name + " is not available with this model or provider. If asked for an image, say so, and do not claim to have made one."

// Guidance is the rule for a prompt whose request is sent with the tool.
const Guidance = "Make an image with " + Name + " when the user asks for one. The user is shown the image itself."

// Summary is the tool's line in the prompt's list of tools.
const Summary = "Generate an image, which is shown to the user"

// Image is what one stored item says about a generation.
type Image struct {
	// ID is the provider's id for the call.
	ID string
	// Failed says no image was made. Reason is then why.
	Failed bool
	Reason string
	// MimeType and Data are the image, set unless Failed.
	MimeType string
	Data     []byte
	// RevisedPrompt is the prompt the provider drew from, when it says.
	RevisedPrompt string
}

// Read returns what a native item says about a generation, and false for an
// item that is not one. An item whose image cannot be read is a failed
// generation with the reason, so that the client is told.
func Read(item transcript.Item) (Image, bool) {
	if item.Native == nil || item.Native.Kind != Kind || len(item.Opaque) == 0 {
		return Image{}, false
	}
	var call struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		Result        string `json:"result"`
		RevisedPrompt string `json:"revised_prompt"`
	}
	var img Image
	if err := json.Unmarshal(item.Opaque, &call); err != nil {
		img.Failed, img.Reason = true, "the stored item cannot be read"
		return img, true
	}
	img.ID, img.RevisedPrompt = call.ID, call.RevisedPrompt
	if call.Status != "" && call.Status != "completed" {
		img.Failed, img.Reason = true, "the image generation ended as "+call.Status
		return img, true
	}
	data, err := base64.StdEncoding.DecodeString(call.Result)
	if err != nil || len(data) == 0 {
		img.Failed, img.Reason = true, "the image generation returned no image data"
		return img, true
	}
	// The MIME type comes from the bytes: the item's output_format names
	// what was asked for, and the client needs what was sent.
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		img.Failed, img.Reason = true, "the image generation returned data that is not an image"
		return img, true
	}
	img.MimeType, img.Data = mime, data
	return img, true
}

// standIn is the text for a generation in a request that cannot carry it.
func standIn(img Image) string {
	if img.Failed {
		return "[" + Name + " failed: " + img.Reason + "]"
	}
	text := "[" + Name + ": an image was generated and shown to the user. It is no longer available to you.]"
	if img.RevisedPrompt != "" {
		text += "\nPrompt used: " + img.RevisedPrompt
	}
	return text
}

// Portable returns items with every generation that the request cannot
// carry replaced by text. A failed generation is always text. A generation goes back as it is only to the
// provider, model and account that made it, and only when the request
// declares the tool. Other items are returned unchanged.
func Portable(items []transcript.Item, providerName string, origin transcript.Origin, declared bool) []transcript.Item {
	var out []transcript.Item
	for i, item := range items {
		img, ok := Read(item)
		// A failed generation has no image, and nothing shows the API takes
		// one back, so it goes as text to every request.
		if !ok || (!img.Failed && declared && item.OpaqueCompatible(providerName, origin)) {
			if out != nil {
				out = append(out, item)
			}
			continue
		}
		if out == nil {
			out = append(make([]transcript.Item, 0, len(items)), items[:i]...)
		}
		out = append(out, transcript.Item{Text: &transcript.Text{Text: standIn(img)}})
	}
	if out == nil {
		return items
	}
	return out
}
