package loop

import (
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/transcript"
)

// BenchmarkConversation builds one request's items from a session of 2000
// entries, as contextItems does for every model call of a turn.
func BenchmarkConversation(b *testing.B) {
	now := time.Now()
	var entries []transcript.Entry
	var parent uuid.UUID
	add := func(item transcript.Item) {
		e := transcript.NewEntry(parent, now, item)
		parent = e.ID
		entries = append(entries, e)
	}
	output := strings.Repeat("output line\n", 20)
	for i := range 400 {
		id := strconv.Itoa(i)
		add(transcript.Item{User: &transcript.User{Parts: []transcript.Part{{Text: "do the next thing"}}}})
		add(transcript.Item{ToolCall: &transcript.ToolCall{ID: id, Name: "bash", Input: []byte(`{"command":"ls"}`)}})
		add(transcript.Item{ToolResult: &transcript.ToolResult{CallID: id, Name: "bash", Outcome: transcript.OutcomeCompleted, Parts: []transcript.Part{{Text: output}}}})
		add(transcript.Item{Text: &transcript.Text{Text: "done"}})
		add(transcript.Item{Usage: &transcript.Usage{Model: "m"}})
	}
	b.ReportAllocs()
	for b.Loop() {
		items, err := compact.Items(entries, "portable")
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := conversationInPlace(items, "[interrupted]"); err != nil {
			b.Fatal(err)
		}
	}
}
