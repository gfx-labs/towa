package provider

import (
	"fmt"

	"github.com/gfx-labs/towa/src/transcript"
)

// LeftOut is the text a tool result holds in place of a part the API cannot
// carry, so that the model learns the tool returned it. A result is stored
// before a request is built from it, so refusing the request instead would
// fail every later turn of the session. api names the API as it reads after
// "over": "the Messages API".
func LeftOut(p transcript.Part, api string) string {
	what, size := "content", 0
	switch {
	case p.Image != nil:
		what, size = "an image"+ofType(p.Image.MimeType), len(p.Image.Data)
	case p.Audio != nil:
		what, size = "audio"+ofType(p.Audio.MimeType), len(p.Audio.Data)
	case p.Resource != nil:
		what, size = "the resource "+p.Resource.URI+ofType(p.Resource.MimeType), len(p.Resource.Data)
	}
	return fmt.Sprintf("[The tool returned %s (%d bytes), which cannot be sent over %s. It is not in this result.]", what, size, api)
}

func ofType(mimeType string) string {
	if mimeType == "" {
		return " of no stated type"
	}
	return " of type " + mimeType
}
