package session

import (
	"context"
	"maps"
	"sync"

	"github.com/gfx-labs/towa/pkg/acp"
	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/store"
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
	// What session/new asked for. None of it changes afterwards.
	dirs         []string
	systemPrompt *acp.SystemPrompt
	mcpServers   []acp.McpServer
	// perTurn are the changes the session's _meta overrides make to each
	// turn.
	perTurn     []func(*loop.Turn)
	overrides   map[string]any
	runtime     Runtime
	stopRuntime context.CancelFunc
	finishOpen  func()
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error

	// slot holds a token while a turn runs.
	slot chan struct{}

	// setMu orders changes to config options with the updates about them.
	setMu sync.Mutex

	mu sync.Mutex
	// cancelBefore is the highest arrival of a cancel or prompt seen so far.
	// A prompt that arrived before it is cancelled.
	cancelBefore uint64
	// turns holds the cancel function of each prompt that is running or
	// waiting to run, by arrival.
	turns  map[uint64]context.CancelFunc
	closed bool
	// values holds the value of every config option, the mode among them.
	values map[acp.SessionConfigId]Value
	title  string
	// allowed are the tools the client allowed for the rest of the session.
	allowed map[string]bool
	// announced is set once the client has been sent the commands.
	announced       bool
	announcePending bool
	// owner is the agent of the connection the session is live on. The
	// host's table lock guards it.
	owner *Agent
	// meta is the session as the store last had it. It is the zero value
	// without a store.
	meta store.Meta
}

func newSession(id acp.SessionId, cwd string) *session {
	return &session{
		id: id, cwd: cwd, slot: make(chan struct{}, 1), turns: map[uint64]context.CancelFunc{},
		allowed: map[string]bool{}, closeDone: make(chan struct{}),
	}
}

// run runs turn for the prompt with the given arrival, once the turns of
// earlier prompts have stopped. It answers with the cancelled stop reason
// when a later cancel or prompt supersedes it, or when ctx ends.
func (s *session) run(ctx context.Context, arrival uint64, turn func(context.Context) (loop.Result, error)) (loop.Result, error) {
	cancelled := loop.Result{StopReason: acp.StopReasonCancelled}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return loop.Result{}, unknownSession(s.id)
	case arrival < s.cancelBefore:
		s.mu.Unlock()
		return cancelled, nil
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
		return cancelled, nil
	}
	defer func() { <-s.slot }()
	// The select can take the slot although ctx has ended.
	if ctx.Err() != nil {
		return cancelled, nil
	}

	result, err := turn(ctx)
	if err != nil && ctx.Err() != nil {
		// A cancelled model request or tool surfaces as an error. The
		// protocol wants the cancelled stop reason, not an error.
		return cancelled, nil
	}
	return result, err
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

// stop cancels the prompt with the given arrival, as a session/cancel for it
// alone would.
func (s *session) stop(arrival uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancelTurn := s.turns[arrival]; cancelTurn != nil {
		cancelTurn()
	}
}

// option returns the value of a config option.
func (s *session) option(id acp.SessionConfigId) (Value, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[id]
	return v, ok
}

// options returns a copy of the value of every config option.
func (s *session) options() map[acp.SessionConfigId]Value {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.values)
}

// close cancels every prompt and returns once the running turn has stopped.
// It keeps the slot, so nothing runs in a closed session.
func (s *session) close(ctx context.Context) error {
	s.beginClose(ctx)
	select {
	case <-s.closeDone:
		return s.closeErr
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// finished reports that close has completed: the turn stopped and the
// runtime's Close returned, with or without an error. Until then the session
// is still draining and stays live.
func (s *session) finished() bool {
	select {
	case <-s.closeDone:
		return true
	default:
		return false
	}
}

func (s *session) beginClose(ctx context.Context) {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		for _, cancelTurn := range s.turns {
			cancelTurn()
		}
		s.mu.Unlock()
		if s.stopRuntime != nil {
			s.stopRuntime()
		}
		// Finish cleanup even if the caller stops waiting for it.
		go func() {
			s.slot <- struct{}{}
			// Setters that already passed lookup finish before a new owner
			// reads metadata. Later setters see closed under this same lock.
			s.setMu.Lock()
			runtime := s.runtime
			s.setMu.Unlock()
			if runtime.Close != nil {
				s.closeErr = runtime.Close(context.WithoutCancel(ctx))
			}
			close(s.closeDone)
		}()
	})
}
