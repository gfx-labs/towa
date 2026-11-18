// Package hostedimage is image generation run by the model's provider: the
// tool definition the Responses API takes, and a reading of the item a
// generation leaves in the transcript. That item is stored whole and only its
// own provider takes it back, so this package also gives the text that stands
// for one in a request to another provider.
package hostedimage

import (
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/transcript"
)

// Name is the provider's name for the tool.
const Name = "image_generation"

// KindCall is the transcript.Native.Kind of the item a generation leaves.
const KindCall = "image_generation_call"

// ErrUnsupported is what Tool returns for a provider that has no hosted
// image generation.
var ErrUnsupported = errors.New("hostedimage: the provider has no hosted image generation")

// Tool returns the hosted image generation tool for a chain entry's provider.
// Only the built-in codex provider, which is OpenAI's Responses API with an
// API key or a ChatGPT login, has it: a configured endpoint that speaks the
// same API may not implement it. A disabled tool is false with no error, and
// an enabled one on any other provider is ErrUnsupported, so the caller can
// say so instead of leaving the model without it.
func Tool(cfg config.ImageGeneration, providerID string) (provider.Tool, bool, error) {
	if !cfg.Enabled {
		return provider.Tool{}, false, nil
	}
	if providerID != config.ProviderCodex {
		return provider.Tool{}, false, ErrUnsupported
	}
	raw, err := json.Marshal(map[string]string{
		"type": Name, "quality": cfg.Quality, "output_format": cfg.OutputFormat,
	}, json.Deterministic(true))
	if err != nil {
		return provider.Tool{}, false, fmt.Errorf("hostedimage: %w", err)
	}
	return provider.Tool{Name: Name, Native: raw, NativeAPI: provider.NativeAPIResponses}, true, nil
}

// Image is what one stored item says about a generation.
type Image struct {
	// ID is the provider's id for the call.
	ID string
	// MimeType and Data are the image, when the generation made one.
	MimeType string
	Data     []byte
	// RevisedPrompt is the prompt the image model used, when it reports one.
	RevisedPrompt string
	// Failure says why there is no image. It is empty when there is one.
	Failure string
}

// Read returns what a native item says about a hosted generation, and false
// for an item that is not one. An item whose image cannot be read is still
// one, with Failure set, so a client is told instead of shown nothing.
func Read(item transcript.Item) (Image, bool) {
	if item.Native == nil || item.Native.Kind != KindCall || len(item.Opaque) == 0 {
		return Image{}, false
	}
	var call struct {
		ID            string `json:"id"`
		Status        string `json:"status"`
		Result        string `json:"result"`
		RevisedPrompt string `json:"revised_prompt"`
	}
	if err := json.Unmarshal(item.Opaque, &call); err != nil {
		return Image{Failure: "the image generation item is not the JSON this client reads"}, true
	}
	out := Image{ID: call.ID, RevisedPrompt: call.RevisedPrompt}
	switch {
	case call.Status == "failed":
		out.Failure = "image generation failed"
	case call.Result == "":
		out.Failure = "image generation returned no image (status " + call.Status + ")"
	default:
		data, err := base64.StdEncoding.DecodeString(call.Result)
		if err != nil {
			out.Failure = "image generation returned data that is not base64"
			break
		}
		// The type is read from the bytes: the item's output_format names a
		// request, and a client needs what the data is.
		mime := http.DetectContentType(data)
		switch mime {
		case "image/png", "image/jpeg", "image/webp":
			out.MimeType, out.Data = mime, data
		default:
			out.Failure = fmt.Sprintf("image generation returned data of type %s, not an image", mime)
		}
	}
	return out, true
}

// standIn is what a request to another provider holds for a generated image.
func standIn(img Image) string {
	text := "[" + Name + ": an image was generated and shown to the user; it is no longer available]"
	if img.Failure != "" {
		text = "[" + Name + ": " + img.Failure + "]"
	}
	if img.RevisedPrompt != "" {
		text += "\nPrompt used: " + img.RevisedPrompt
	}
	return text
}

// Portable returns items with every generated-image item that the request
// cannot carry replaced by text. The item goes back as it is only to the
// provider, model and account that made it, and only when the request
// declares the hosted tool. Other items are returned unchanged.
func Portable(items []transcript.Item, providerName string, origin transcript.Origin, declared bool) []transcript.Item {
	var out []transcript.Item
	for i, item := range items {
		img, ok := Read(item)
		if !ok || (declared && item.OpaqueCompatible(providerName, origin)) {
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
