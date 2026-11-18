package loop_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json/v2"
	"image"
	"image/png"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/provider/hostedimage"
	"github.com/gfx-labs/towa/src/transcript"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	return buf.Bytes()
}

func imageItem(t *testing.T, call map[string]any) transcript.Item {
	t.Helper()
	call["type"] = hostedimage.Kind
	raw, err := json.Marshal(call)
	require.NoError(t, err)
	return transcript.Item{Native: &transcript.Native{Kind: hostedimage.Kind}, Opaque: raw}
}

// A client is shown the generated image itself, with the type of what was
// sent rather than the type that was asked for.
func TestUpdatesShowAGeneratedImage(t *testing.T) {
	data := pngBytes(t)
	item := imageItem(t, map[string]any{"id": "ig_1", "status": "completed", "output_format": "webp", "result": base64.StdEncoding.EncodeToString(data)})

	updates, err := loop.Mapper{}.Updates(item, &loop.Cost{})
	require.NoError(t, err)
	assert.Equal(t, []map[string]any{{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"type": "image", "mimeType": "image/png", "data": base64.StdEncoding.EncodeToString(data)},
	}}, wire(t, updates))
}

// A generation that made no image is shown as a failed call, not as an
// empty image.
func TestUpdatesShowAFailedImageGeneration(t *testing.T) {
	item := imageItem(t, map[string]any{"id": "ig_2", "status": "failed"})
	updates, err := loop.Mapper{}.Updates(item, &loop.Cost{})
	require.NoError(t, err)
	require.Len(t, updates, 1)
	require.NotNil(t, updates[0].ToolCall)
	assert.Equal(t, "failed", string(updates[0].ToolCall.Status))
}
