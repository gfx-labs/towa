package acp

import (
	"context"
	"sync"
)

type afterResponseKey struct{}

type afterResponse struct {
	mu        sync.Mutex
	finished  bool
	callbacks []func()
}

// AfterResponse queues fn to run after the response to ctx's request has
// been written, in registration order, on the request handler's goroutine.
// It also runs after an error response or a failed write. Serve waits for
// the callbacks before returning. A callback must not block indefinitely.
// It reports false if fn is nil, ctx is not a request handler's context,
// or the response has already been written.
func AfterResponse(ctx context.Context, fn func()) bool {
	a, ok := ctx.Value(afterResponseKey{}).(*afterResponse)
	if !ok || fn == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finished {
		return false
	}
	a.callbacks = append(a.callbacks, fn)
	return true
}

func (a *afterResponse) run() {
	a.mu.Lock()
	a.finished = true
	callbacks := a.callbacks
	a.callbacks = nil
	a.mu.Unlock()
	for _, fn := range callbacks {
		fn()
	}
}
