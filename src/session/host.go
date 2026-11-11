package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/gfx-labs/acpharness/pkg/acp"
	"github.com/gfx-labs/acpharness/src/loop"
	"github.com/gfx-labs/acpharness/src/store"
)

// Host is what a process has once for all its connections: the store, the
// registry and the table of sessions that are live, each on one connection.
// A process that serves many connections makes one Host and one Agent per
// connection from it.
type Host struct {
	*shared
}

// shared is the state of a Host. Agents embed it, so that an agent reads
// the process's settings as its own.
type shared struct {
	version  string
	turn     TurnFunc
	logger   *slog.Logger
	tracer   trace.Tracer
	turns    metric.Int64Counter
	repairs  metric.Int64Counter
	registry *Registry
	supports Supports
	store    store.Store
	mapper   loop.Mapper
	// pageSize is session.list_page_size.
	pageSize int

	liveMu sync.Mutex
	// live holds the sessions in memory. Each belongs to the agent in its
	// owner field.
	live map[acp.SessionId]*session
	// busy holds a lock per session that is being loaded, resumed, deleted
	// or titled.
	busy map[acp.SessionId]*idLock
}

type idLock struct {
	mu   sync.Mutex
	refs int
}

// NewHost returns a host with no sessions.
func NewHost(o Options) (*Host, error) {
	turns, err := o.Meter.Int64Counter("acpharness.turns",
		metric.WithDescription("Prompt turns finished, by stop reason."),
		metric.WithUnit("{turn}"))
	if err != nil {
		return nil, err
	}
	repairs, err := o.Meter.Int64Counter("acpharness.session.repairs",
		metric.WithDescription("Session logs whose torn tails were set aside."),
		metric.WithUnit("{repair}"))
	if err != nil {
		return nil, err
	}
	registry := &Registry{}
	if o.Registry != nil {
		registry = o.Registry.clone()
	}
	if err := errors.Join(
		registry.AddConfigOption(modeOption(acp.SessionConfigValueId(o.Session.Mode))),
		registry.AddOverride(modeOverride()),
		registry.AddCommand(modeCommand()),
	); err != nil {
		return nil, err
	}
	if o.Supports.ContextOverrides {
		if err := errors.Join(
			registry.AddOverride(pathsOverride("context_files", func(turn *loop.Turn, paths []string) { turn.ContextFiles = paths })),
			registry.AddOverride(pathsOverride("skills_dirs", func(turn *loop.Turn, paths []string) { turn.SkillsDirs = paths })),
		); err != nil {
			return nil, err
		}
	}
	return &Host{&shared{
		version:  o.Version,
		turn:     o.Turn,
		logger:   o.Logger,
		tracer:   o.Tracer,
		turns:    turns,
		repairs:  repairs,
		registry: registry,
		supports: o.Supports,
		store:    o.Store,
		mapper:   o.Mapper,
		pageSize: o.Session.ListPageSize,
		live:     map[acp.SessionId]*session{},
		busy:     map[acp.SessionId]*idLock{},
	}}, nil
}

// NewAgent returns the agent for one connection. Call SetClient before the
// connection is served, and Close once it has ended.
func (h *Host) NewAgent() *Agent { return &Agent{shared: h.shared} }

// ServeConn serves one connection: newline-delimited JSON-RPC read from and
// written to rw. It returns when the client hangs up or ctx is cancelled,
// with the error acp's Serve gives: nil when the client closed between
// messages. The connection's turns are then cancelled and its sessions
// taken out of memory. With a store they stay there, for a later
// connection to load.
//
// A read that is blocked when ctx is cancelled stays blocked until rw is
// closed, which is the caller's to do.
func (h *Host) ServeConn(ctx context.Context, rw io.ReadWriter) error {
	agent := h.NewAgent()
	conn := acp.NewAgentSideConnection(agent, rw, rw, h.logger)
	agent.SetClient(conn)
	err := conn.Serve(ctx)
	// Serve has waited for the turns, so this does not wait on a client
	// that is gone. ctx may be cancelled already.
	return errors.Join(err, agent.Close(context.WithoutCancel(ctx)))
}

// Close ends the agent's connection: it cancels the turns of the sessions
// that are live on it, waits for them and takes the sessions out of memory.
// The agent accepts no session afterwards.
func (a *Agent) Close(ctx context.Context) error {
	a.liveMu.Lock()
	a.ended = true
	var owned []*session
	for id, s := range a.live {
		if s.owner == a {
			owned = append(owned, s)
			delete(a.live, id)
		}
	}
	a.liveMu.Unlock()
	var errs []error
	for _, s := range owned {
		errs = append(errs, s.close(ctx))
	}
	return errors.Join(errs...)
}

// lookup returns a live session and the agent it is live on.
func (h *shared) lookup(id acp.SessionId) (*session, *Agent) {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	s := h.live[id]
	if s == nil {
		return nil, nil
	}
	return s, s.owner
}

// take removes a live session from the table, whichever agent has it.
func (h *shared) take(id acp.SessionId) *session {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	s := h.live[id]
	delete(h.live, id)
	return s
}

// takeOwned removes a session from the table if it is live on owner.
func (h *shared) takeOwned(owner *Agent, id acp.SessionId) *session {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	s := h.live[id]
	if s == nil || s.owner != owner {
		return nil
	}
	delete(h.live, id)
	return s
}

// put makes a session live on owner. It fails for an agent whose
// connection has ended, which would leave a session nobody can reach.
func (h *shared) put(s *session, owner *Agent) error {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	if owner.ended {
		return errors.New("session: the connection has ended")
	}
	s.owner = owner
	h.live[s.id] = s
	return nil
}

// lock waits for the session's lock and returns the function that releases
// it. Loads, resumes, deletes and title changes of one session hold it, so
// they happen one at a time. Other sessions are not held up.
func (h *shared) lock(id acp.SessionId) func() {
	h.liveMu.Lock()
	l := h.busy[id]
	if l == nil {
		l = &idLock{}
		h.busy[id] = l
	}
	l.refs++
	h.liveMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		h.liveMu.Lock()
		if l.refs--; l.refs == 0 {
			delete(h.busy, id)
		}
		h.liveMu.Unlock()
	}
}
