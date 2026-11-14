package loop

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gfx-labs/towa/src/agent/hooks"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/prompt/compact"
	"github.com/gfx-labs/towa/src/provider"
	"github.com/gfx-labs/towa/src/store"
	"github.com/gfx-labs/towa/src/tools"
	"github.com/gfx-labs/towa/src/tools/jobs"
	"github.com/gfx-labs/towa/src/tools/repair"
)

// Options is what a turn is built from. Every field is required unless it
// says otherwise.
type Options struct {
	// Resolve returns what a turn needs from the model the session chose.
	// It is called once per turn, before the first request, with
	// Turn.Model as it is: an empty name means the session chose none, and
	// the resolver owns the default.
	Resolve func(ctx context.Context, model string) (Resolved, error)
	// NewCompactor returns the controller for this turn's session. Nil
	// disables automatic compaction. Close cancels pending work at turn end.
	NewCompactor func(context.Context, Turn, Resolved) (*compact.Controller, error)
	Store        store.Store
	// System returns the system prompt's sections for a turn.
	System func(ctx context.Context, t Turn) ([]provider.SystemBlock, error)
	Limits config.Limits
	// Now is the clock: entry times and the turn's wall time come from it.
	Now func() time.Time
	// Repair is tools.repair.enabled: fix the known ways a model gets a
	// tool's argument wrong before validating it.
	Repair bool
	// Gate is tools.gate: when a tool's result is withheld from the model.
	Gate config.ToolsGate
	// Hooks runs the configured hooks. It may have none.
	Hooks  *hooks.Runner
	Logger *slog.Logger
	// Jobs are the session's background commands. A turn the model ends
	// while one runs waits for it, and no job outlives the turn that
	// started it. Nil when the session has none.
	Jobs *jobs.Set
}

// Resolved is a model as one turn uses it.
type Resolved struct {
	// Provider answers the turn's model requests. It is the router over the
	// model's chain, which sets each request's model and credential.
	Provider provider.Provider
	// Tools is the session's tool set.
	Tools tools.Set
	// Schemas is the compiled input schema of each tool in Tools that has
	// one, by tool name. Nil means the loop compiles them, once a turn.
	Schemas map[string]*repair.Schema
	// Model is the provider's own name for the model, of the chain's first
	// entry. It is for logs: the router names the model of each request.
	Model string
	// ProviderName identifies the first entry's provider for native context.
	// A router supplies the actual entry's ProviderID before each request.
	ProviderName string
	// ContextSize is the model's context window in tokens, for usage
	// updates. Zero means it is not known, and no usage update is sent.
	ContextSize int64
	// MaxOutputTokens and Effort are sent with every request, and not sent
	// when zero.
	MaxOutputTokens int64
	Effort          string
	// Interruption is the text that tells the model the turn before was
	// interrupted. It is required.
	Interruption string
}

// Loop runs prompt turns. It keeps nothing between turns: a session's
// conversation is read from the store when a turn starts.
type Loop struct {
	o Options
}

// New returns a loop over its dependencies.
func New(o Options) (*Loop, error) {
	switch {
	case o.Resolve == nil:
		return nil, errors.New("loop: Resolve is required")
	case o.Store == nil:
		return nil, errors.New("loop: Store is required")
	case o.System == nil:
		return nil, errors.New("loop: System is required")
	case o.Now == nil:
		return nil, errors.New("loop: Now is required")
	case o.Hooks == nil:
		return nil, errors.New("loop: Hooks is required")
	case o.Logger == nil:
		return nil, errors.New("loop: Logger is required")
	}
	return &Loop{o: o}, nil
}
