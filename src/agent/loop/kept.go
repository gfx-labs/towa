package loop

import (
	"sync"
	"time"
	"uuid"

	"github.com/gfx-labs/towa/src/transcript"
)

// Kept holds one session's entries between its turns, so that a turn soon
// after the last does not read the whole log again. It lets them go once
// the session has had no turn for its idle time. The zero value is not
// usable: call NewKept.
type Kept struct {
	idle time.Duration

	mu sync.Mutex
	// id is the session the entries are of. A Loop serves one session, and
	// a turn of another session ignores what is kept.
	id      uuid.UUID
	entries []transcript.Entry
	cost    Cost
	timer   *time.Timer
}

// NewKept returns a Kept that lets a session's entries go after idle
// without a turn. Zero keeps nothing.
func NewKept(idle time.Duration) *Kept {
	return &Kept{idle: idle}
}

// take returns what is kept for the session and empties k, so that a turn
// that fails keeps nothing it may have changed half way.
func (k *Kept) take(id uuid.UUID) ([]transcript.Entry, Cost, bool) {
	if k == nil {
		return nil, Cost{}, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	entries, cost, ok := k.entries, k.cost, k.entries != nil && k.id == id
	k.clear()
	return entries, cost, ok
}

// put keeps a session's entries after a turn that stored all it meant to.
func (k *Kept) put(id uuid.UUID, entries []transcript.Entry, cost Cost) {
	if k == nil || k.idle <= 0 || len(entries) == 0 {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.id, k.entries, k.cost = id, entries, cost
	k.timer = time.AfterFunc(k.idle, k.Drop)
}

// Drop lets the kept entries go.
func (k *Kept) Drop() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.clear()
}

func (k *Kept) clear() {
	if k.timer != nil {
		k.timer.Stop()
		k.timer = nil
	}
	k.entries, k.cost = nil, Cost{}
}
