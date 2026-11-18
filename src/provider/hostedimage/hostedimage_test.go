package hostedimage_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/provider/hostedimage"
	"github.com/gfx-labs/towa/src/transcript"
)

func TestToolIsOfferedOnlyOnCodexModelsThatTakeHostedTools(t *testing.T) {
	cfg := config.ImageGeneration{Enabled: true, OutputFormat: "png"}
	tool, ok, err := hostedimage.Tool(cfg, config.ProviderCodex, "gpt-6.1-sol")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, provider.NativeAPIResponses, tool.NativeAPI)
	var def map[string]string
	require.NoError(t, json.Unmarshal(tool.Native, &def))
	assert.Equal(t, map[string]string{"type": "image_generation", "output_format": "png"}, def)

	for name, c := range map[string]struct {
		cfg      config.ImageGeneration
		provider string
		model    string
	}{
		"disabled":      {config.ImageGeneration{OutputFormat: "png"}, config.ProviderCodex, "gpt-6.1-sol"},
		"claude":        {cfg, config.ProviderClaude, "claude-opus-5.5"},
		"configured":    {cfg, "openrouter", "openai/gpt-6.1-sol"},
		"codex-named":   {cfg, config.ProviderCodex, "gpt-6-codex-mini"},
		"other casing":  {cfg, config.ProviderCodex, "GPT-6-Codex"},
		"empty account": {cfg, "", "gpt-6.1-sol"},
	} {
		_, ok, err := hostedimage.Tool(c.cfg, c.provider, c.model)
		require.NoError(t, err, name)
		assert.False(t, ok, name)
	}
}

func generation(t *testing.T, id string, data []byte, own bool) transcript.Item {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"type": hostedimage.Kind, "id": id, "status": "completed", "result": base64.StdEncoding.EncodeToString(data),
		"revised_prompt": "a red apple",
	})
	require.NoError(t, err)
	item := transcript.Item{Native: &transcript.Native{Kind: hostedimage.Kind}, Opaque: raw}
	if own {
		item.Provider, item.Origin = "codex", &transcript.Origin{Model: "m", Account: "a"}
	}
	return item
}

func TestReadSniffsTheImageType(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	img, ok := hostedimage.Read(generation(t, "ig_1", buf.Bytes(), true))
	require.True(t, ok)
	assert.False(t, img.Failed)
	assert.Equal(t, "image/png", img.MimeType)
	assert.Equal(t, buf.Bytes(), img.Data)
	assert.Equal(t, "ig_1", img.ID)

	img, ok = hostedimage.Read(generation(t, "ig_2", []byte("not an image"), true))
	require.True(t, ok)
	assert.True(t, img.Failed, "data that is not an image is a failed generation")

	_, ok = hostedimage.Read(transcript.Item{Native: &transcript.Native{Kind: "web_search_call"}, Opaque: []byte(`{}`)})
	assert.False(t, ok)
}

// The image goes back only to the request that made it and declares the
// tool. Any other request is told in text that an image was made.
func TestPortableKeepsAGenerationOnlyForItsOwnRequest(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	items := []transcript.Item{{Text: &transcript.Text{Text: "hi"}}, generation(t, "ig_1", buf.Bytes(), true)}
	own := transcript.Origin{Model: "m", Account: "a"}

	assert.Equal(t, items, hostedimage.Portable(items, "codex", own, true))
	for name, got := range map[string][]transcript.Item{
		"tool not declared": hostedimage.Portable(items, "codex", own, false),
		"other model":       hostedimage.Portable(items, "codex", transcript.Origin{Model: "n", Account: "a"}, true),
		"other provider":    hostedimage.Portable(items, "claude", own, true),
	} {
		require.Len(t, got, 2, name)
		assert.Equal(t, items[0], got[0], name)
		require.NotNil(t, got[1].Text, name)
		assert.Contains(t, got[1].Text.Text, "a red apple", name)
		assert.NotContains(t, got[1].Text.Text, base64.StdEncoding.EncodeToString(buf.Bytes()), name)
	}
}

// A generation that made no image has nothing to send back, so every
// request gets its text, the one that made it included.
func TestPortableNeverReturnsAFailedGeneration(t *testing.T) {
	raw, err := json.Marshal(map[string]any{"type": hostedimage.Kind, "id": "ig_3", "status": "failed"})
	require.NoError(t, err)
	failed := transcript.Item{
		Native: &transcript.Native{Kind: hostedimage.Kind}, Opaque: raw,
		Provider: "codex", Origin: &transcript.Origin{Model: "m", Account: "a"},
	}
	got := hostedimage.Portable([]transcript.Item{failed}, "codex", transcript.Origin{Model: "m", Account: "a"}, true)
	require.Len(t, got, 1)
	require.NotNil(t, got[0].Text)
}
