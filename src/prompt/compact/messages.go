package compact

import (
	"uuid"

	"github.com/gfx-labs/towa/src/transcript"
)

// Roles of a conversation item.
const (
	roleNone      = 0
	roleUser      = 'u'
	roleAssistant = 'a'
)

// role says which side of the conversation an item is on. Usage and
// ModelSwitch items are on neither.
func role(item transcript.Item) byte {
	switch {
	case item.User != nil, item.ToolResult != nil, item.Interrupted != nil:
		return roleUser
	case item.Text != nil, item.Thinking != nil, item.ToolCall != nil, item.Native != nil:
		return roleAssistant
	default:
		return roleNone
	}
}

// span is one message: a run of entries of one role. An entry with no role
// belongs to the message it follows.
type span struct {
	role       byte
	start, end int // entries[start:end]
}

func spans(entries []transcript.Entry) []span {
	var out []span
	for i, e := range entries {
		r := role(e.Item)
		if r == roleNone {
			if len(out) > 0 {
				out[len(out)-1].end = i + 1
			}
			continue
		}
		if n := len(out); n > 0 && out[n-1].role == r {
			out[n-1].end = i + 1
			continue
		}
		out = append(out, span{role: r, start: i, end: i + 1})
	}
	return out
}

// Messages counts the messages in entries, as a provider would see them: a
// run of items of one role is one message.
func Messages(entries []transcript.Entry) int {
	return len(spans(entries))
}

// Cut chooses where a compaction of active divides it, keeping the latest
// keep messages. It returns the number of leading entries to replace, and
// the id of the first kept entry, which is the nil id when every entry is
// replaced. A count of zero means there is nothing to compact.
//
// The division never falls inside a message and never separates a tool
// result from its call: the kept part grows backwards until both hold.
func Cut(active []transcript.Entry, keep int) (replace int, firstKept uuid.UUID) {
	msgs := spans(active)
	if len(msgs) <= keep {
		return 0, uuid.Nil()
	}
	if keep <= 0 {
		// Leading entries with no role stay with the replaced part.
		return len(active), uuid.Nil()
	}
	cut := pullBack(active, msgs, msgs[len(msgs)-keep].start)
	if cut == 0 {
		return 0, uuid.Nil()
	}
	return cut, active[cut].ID
}

// pullBack moves cut back until it is on a message boundary and every tool
// result after it has its call after it too.
func pullBack(active []transcript.Entry, msgs []span, cut int) int {
	calls := map[string]int{}
	for i, e := range active {
		if e.Item.ToolCall != nil {
			calls[e.Item.ToolCall.ID] = i
		}
	}
	for moved := true; moved; {
		moved = false
		for _, e := range active[cut:] {
			if e.Item.ToolResult == nil {
				continue
			}
			if at, ok := calls[e.Item.ToolResult.CallID]; ok && at < cut {
				cut, moved = at, true
			}
		}
		// Back to the start of the message the cut landed in.
		for _, m := range msgs {
			if m.start < cut && cut < m.end {
				cut, moved = m.start, true
			}
		}
	}
	return cut
}
