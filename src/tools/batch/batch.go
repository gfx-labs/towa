// Package batch runs several independent tool calls as one call, for a
// model that does not emit parallel calls.
package batch

import (
	"context"
	"encoding/json/jsontext"
	"fmt"
	"slices"
	"sync"

	"github.com/gfx-labs/acpharness/src/config"
	"github.com/gfx-labs/acpharness/src/tools"
	"github.com/gfx-labs/acpharness/src/transcript"
)

// Call is one call inside a batch.
type Call struct {
	// Name is the tool's name as the model knows it.
	Name string
	// Input is the tool's argument as the model wrote it.
	Input jsontext.Value
}

// Plan is what scheduling needs to know about one call.
type Plan struct {
	Effect tools.Effect
	// WritePaths are the absolute paths a call with Effect Writes will
	// write, from the tool's Describe. A writing call with none is taken to
	// write anywhere.
	WritePaths []string
	// ReadPaths are the absolute paths the call is known to read. A call
	// does not run at the same time as one that writes a path it reads.
	ReadPaths []string
}

// Dispatcher runs the calls of a batch. The loop supplies it, so a call in
// a batch goes through the same repair, validation, permission and gating
// as a call on its own.
type Dispatcher struct {
	// Plan looks a call up. An error fails that call and the others still
	// run: an unknown tool, an argument that does not validate, or a batch
	// inside a batch, which the dispatcher refuses.
	Plan func(index int, call Call) (Plan, error)
	// Run runs a call.
	Run func(ctx context.Context, index int, call Call) (tools.Result, error)
}

// Item is how one call of a batch ended.
type Item struct {
	// Started says Run was called.
	Started bool
	// Result is what Run returned. A call that was not started because the
	// batch was cancelled has only Outcome set, to OutcomeCancelled.
	Result tools.Result
	// Err is the error from Plan or Run. The call counts as failed.
	Err error
}

// Kind says what is wrong with a batch.
type Kind string

// Kinds of an Error.
const (
	// KindEmpty: the batch holds no calls.
	KindEmpty Kind = "empty"
	// KindTooMany: the batch holds Count calls and Limit is the most.
	KindTooMany Kind = "too_many"
)

// Error is a batch the model has to correct. Nothing was run.
type Error struct {
	Kind  Kind
	Count int
	Limit int
}

func (e *Error) Error() string { return "batch: " + string(e.Kind) }

// Run runs the calls and returns one Item for each, in the order given. A
// call starts once every earlier call it conflicts with has ended. The rule
// is the loop's own for parallel calls (loop.Conflicts):
//
//   - a call that executes runs alone: after every call before it and
//     before every call after it, because what it touches is not known;
//   - a call that writes without saying where is treated the same way;
//   - two writes that share a path run in the order given;
//   - a call that reads a path does not overlap one that writes it;
//   - calls that only read run at the same time as each other.
//
// Paths are compared as text, so the dispatcher gives them in one form.
//
// When ctx is cancelled, calls that have not started are not started.
func Run(ctx context.Context, cfg config.ToolsBatch, d Dispatcher, calls []Call) ([]Item, error) {
	switch {
	case len(calls) == 0:
		return nil, &Error{Kind: KindEmpty}
	case len(calls) > cfg.MaxCalls:
		return nil, &Error{Kind: KindTooMany, Count: len(calls), Limit: cfg.MaxCalls}
	}
	items := make([]Item, len(calls))
	plans := make([]Plan, len(calls))
	for i, call := range calls {
		plan, err := d.Plan(i, call)
		if err != nil {
			items[i] = Item{Result: tools.Result{Outcome: transcript.OutcomeFailed}, Err: fmt.Errorf("batch: call %d: %w", i, err)}
			// A call that will not run still holds its place in the order
			// as a call that touches nothing.
			plan = Plan{Effect: tools.ReadOnly}
		}
		plans[i] = plan
	}

	done := make([]chan struct{}, len(calls))
	for i := range done {
		done[i] = make(chan struct{})
	}
	var wg sync.WaitGroup
	for i, call := range calls {
		if items[i].Err != nil {
			close(done[i])
			continue
		}
		var waits []chan struct{}
		for j := range i {
			if conflicts(plans[j], plans[i]) {
				waits = append(waits, done[j])
			}
		}
		wg.Go(func() {
			defer close(done[i])
			for _, earlier := range waits {
				<-earlier
			}
			if ctx.Err() != nil {
				items[i] = Item{Result: tools.Result{Outcome: transcript.OutcomeCancelled}}
				return
			}
			res, err := d.Run(ctx, i, call)
			if err != nil {
				res.Outcome = transcript.OutcomeFailed
				err = fmt.Errorf("batch: call %d: %w", i, err)
			}
			items[i] = Item{Started: true, Result: res, Err: err}
		})
	}
	wg.Wait()
	return items, nil
}

// conflicts says whether two calls must not run at the same time.
func conflicts(a, b Plan) bool {
	if exclusive(a) || exclusive(b) {
		return true
	}
	return overlap(a.WritePaths, b.WritePaths) || overlap(a.WritePaths, b.ReadPaths) || overlap(a.ReadPaths, b.WritePaths)
}

func exclusive(p Plan) bool {
	return p.Effect == tools.Executes || (p.Effect == tools.Writes && len(p.WritePaths) == 0)
}

func overlap(a, b []string) bool {
	for _, p := range a {
		if slices.Contains(b, p) {
			return true
		}
	}
	return false
}

// Outcome is the outcome of the batch as one call: completed when every
// call completed, unknown when any call's is, cancelled when any call was
// cancelled and none is unknown, and failed otherwise.
func Outcome(items []Item) transcript.Outcome {
	out := transcript.OutcomeCompleted
	for _, item := range items {
		switch item.Result.Outcome {
		case transcript.OutcomeUnknown:
			return transcript.OutcomeUnknown
		case transcript.OutcomeCancelled:
			out = transcript.OutcomeCancelled
		case transcript.OutcomeCompleted:
		default:
			if out == transcript.OutcomeCompleted {
				out = transcript.OutcomeFailed
			}
		}
	}
	return out
}
