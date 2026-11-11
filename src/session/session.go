package session

import (
	"context"
	"sync"

	"github.com/gfx-labs/acpharness/src/acp"
)

// session is one conversation. At most one turn runs at a time.
//
// Prompt requests are handled concurrently, so two prompts and a cancel can
// reach the session in any order. Their arrival numbers, the order in which
// the connection read them, decide the outcome: a cancel or a prompt cancels
// every prompt that arrived before it.
type session struct {
	id  acp.SessionId
	cwd string

	// slot holds a token while a turn runs.
	slot chan struct{}

	mu sync.Mutex
	// cancelBefore is the highest arrival of a cancel or prompt seen so far.
	// A prompt that arrived before it is cancelled.
	cancelBefore uint64
	// turns holds the cancel function of each prompt that is running or
	// waiting to run, by arrival.
	turns  map[uint64]context.CancelFunc
	closed bool
}

func newSession(id acp.SessionId, cwd string) *session {
	return &session{id: id, cwd: cwd, slot: make(chan struct{}, 1), turns: map[uint64]context.CancelFunc{}}
}

// run runs turn for the prompt with the given arrival, once the turns of
// earlier prompts have stopped. It answers with the cancelled stop reason
// when a later cancel or prompt supersedes it, or when ctx ends.
func (s *session) run(ctx context.Context, arrival uint64, turn func(context.Context) (acp.StopReason, error)) (acp.StopReason, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return "", unknownSession(s.id)
	case arrival < s.cancelBefore:
		s.mu.Unlock()
		return acp.StopReasonCancelled, nil
	}
	s.cancelBefore = arrival
	// Every prompt still here arrived earlier: a later one would have
	// raised cancelBefore above this arrival.
	for _, cancelEarlier := range s.turns {
		cancelEarlier()
	}
	s.turns[arrival] = cancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.turns, arrival)
		s.mu.Unlock()
	}()

	select {
	case s.slot <- struct{}{}:
	case <-ctx.Done():
		return acp.StopReasonCancelled, nil
	}
	defer func() { <-s.slot }()
	// The select can take the slot although ctx has ended.
	if ctx.Err() != nil {
		return acp.StopReasonCancelled, nil
	}

	reason, err := turn(ctx)
	if err != nil && ctx.Err() != nil {
		// A cancelled model request or tool surfaces as an error. The
		// protocol wants the cancelled stop reason, not an error.
		return acp.StopReasonCancelled, nil
	}
	return reason, err
}

// cancel cancels the prompts that arrived before arrival, including ones
// whose handler has not reached run yet.
func (s *session) cancel(arrival uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if arrival > s.cancelBefore {
		s.cancelBefore = arrival
	}
	for n, cancelTurn := range s.turns {
		if n < arrival {
			cancelTurn()
		}
	}
}

// close cancels every prompt and returns once the running turn has stopped.
// It keeps the slot, so nothing runs in a closed session.
func (s *session) close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	for _, cancelTurn := range s.turns {
		cancelTurn()
	}
	s.mu.Unlock()
	select {
	case s.slot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}
