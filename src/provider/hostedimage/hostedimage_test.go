package hostedimage_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/hostedimage"
	"github.com/gfx-labs/towa/src/transcript"
)

func encoded(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	img.Set(1, 1, color.RGBA{R: 200, A: 255})
	var b bytes.Buffer
	switch format {
	case "png":
		require.NoError(t, png.Encode(&b, img))
	case "jpeg":
		require.NoError(t, jpeg.Encode(&b, img, nil))
	}
	return b.Bytes()
}

// call is an image_generation_call item as the Responses API returns one
// when the tool has run.
func call(t *testing.T, fields map[string]any) transcript.Item {
	t.Helper()
	fields["type"] = hostedimage.KindCall
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return transcript.Item{
		Provider: "openai", Origin: &transcript.Origin{Model: "m", Account: "a"},
		Native: &transcript.Native{Kind: hostedimage.KindCall}, Opaque: jsontext.Value(raw),
	}
}

func TestToolIsOnlyForTheBuiltInResponsesProvider(t *testing.T) {
	cfg := config.ImageGeneration{Enabled: true, Quality: "low", OutputFormat: "jpeg"}

	tool, ok, err := hostedimage.Tool(cfg, config.ProviderCodex)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, provider.NativeAPIResponses, tool.NativeAPI)
	require.JSONEq(t, `{"type":"image_generation","quality":"low","output_format":"jpeg"}`, string(tool.Native))

	// A provider without the tool is reported, not left out silently.
	for _, id := range []string{config.ProviderClaude, "openrouter"} {
		_, ok, err = hostedimage.Tool(cfg, id)
		require.ErrorIs(t, err, hostedimage.ErrUnsupported, id)
		require.False(t, ok)
	}

	cfg.Enabled = false
	_, ok, err = hostedimage.Tool(cfg, config.ProviderClaude)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestReadGivesADecodableImageWithItsRealType(t *testing.T) {
	for _, format := range []string{"png", "jpeg"} {
		data := encoded(t, format)
		// output_format is what was asked for. The type is the data's own.
		img, ok := hostedimage.Read(call(t, map[string]any{
			"id": "ig_1", "status": "completed", "output_format": "webp",
			"result": base64.StdEncoding.EncodeToString(data), "revised_prompt": "a red apple",
		}))
		require.True(t, ok)
		require.Empty(t, img.Failure)
		require.Equal(t, "ig_1", img.ID)
		require.Equal(t, "image/"+format, img.MimeType)
		require.Equal(t, "a red apple", img.RevisedPrompt)
		decoded, _, err := image.Decode(bytes.NewReader(img.Data))
		require.NoError(t, err)
		require.Equal(t, 4, decoded.Bounds().Dx())
	}
}

func TestReadSaysWhyThereIsNoImage(t *testing.T) {
	notAnImage := base64.StdEncoding.EncodeToString([]byte("plain text, not an image"))
	for name, fields := range map[string]map[string]any{
		"failed":       {"id": "ig_1", "status": "failed", "result": nil},
		"empty":        {"id": "ig_1", "status": "completed", "result": ""},
		"not base64":   {"id": "ig_1", "status": "completed", "result": "%%%"},
		"not an image": {"id": "ig_1", "status": "completed", "result": notAnImage},
	} {
		img, ok := hostedimage.Read(call(t, fields))
		require.True(t, ok, name)
		require.NotEmpty(t, img.Failure, name)
		require.Empty(t, img.Data, name)
	}
	_, ok := hostedimage.Read(transcript.Item{Native: &transcript.Native{Kind: "web_search_call"}, Opaque: jsontext.Value(`{}`)})
	require.False(t, ok)
	_, ok = hostedimage.Read(transcript.Item{Text: &transcript.Text{Text: "hi"}})
	require.False(t, ok)
}

func TestPortableSendsTheItemOnlyWhereItCanBeRead(t *testing.T) {
	item := call(t, map[string]any{
		"id": "ig_1", "status": "completed", "result": base64.StdEncoding.EncodeToString(encoded(t, "png")),
		"revised_prompt": "a red apple",
	})
	text := transcript.Item{Text: &transcript.Text{Text: "here"}}
	items := []transcript.Item{text, item, text}
	same := transcript.Origin{Model: "m", Account: "a"}

	require.Equal(t, items, hostedimage.Portable(items, "openai", same, true))

	for name, got := range map[string][]transcript.Item{
		"tool not declared": hostedimage.Portable(items, "openai", same, false),
		"other provider":    hostedimage.Portable(items, "anthropic", same, true),
		"other model":       hostedimage.Portable(items, "openai", transcript.Origin{Model: "n", Account: "a"}, true),
		"other account":     hostedimage.Portable(items, "openai", transcript.Origin{Model: "m", Account: "b"}, true),
	} {
		require.Len(t, got, 3, name)
		require.Equal(t, text, got[0], name)
		require.Nil(t, got[1].Native, name)
		require.Empty(t, got[1].Opaque, name)
		require.Contains(t, got[1].Text.Text, "a red apple", name)
		require.NotContains(t, got[1].Text.Text, "iVBOR", name, "the image data stays out of the text")
	}
	// The input is not modified.
	require.NotNil(t, items[1].Native)
}
