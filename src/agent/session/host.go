package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	acp "github.com/gfx-labs/go-acp"

	"github.com/gfx-labs/towa/src/agent/loop"
	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/store"
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
	version string
	open    func(context.Context, loop.Turn) (Runtime, error)
	logger  *slog.Logger
	tracer  trace.Tracer
	turns   metric.Int64Counter
	repairs metric.Int64Counter
	// settings is the registry and the session section of the
	// configuration, which Reload replaces.
	settings atomic.Pointer[settings]
	supports Supports
	store    store.Store

	liveMu   sync.Mutex
	ending   bool
	opening  sync.WaitGroup
	lifetime context.Context
	stop     context.CancelFunc
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
	if o.Open == nil {
		return nil, errors.New("session: Open is required")
	}
	turns, err := o.Meter.Int64Counter("towa.turns",
		metric.WithDescription("Prompt turns finished, by stop reason."),
		metric.WithUnit("{turn}"))
	if err != nil {
		return nil, err
	}
	repairs, err := o.Meter.Int64Counter("towa.session.repairs",
		metric.WithDescription("Session logs whose torn tails were set aside."),
		metric.WithUnit("{repair}"))
	if err != nil {
		return nil, err
	}
	registry, err := newRegistry(o.Registry, o.Session, o.Supports)
	if err != nil {
		return nil, err
	}
	lifetime, stop := context.WithCancel(context.Background())
	h := &Host{&shared{
		version:  o.Version,
		open:     o.Open,
		lifetime: lifetime,
		stop:     stop,
		logger:   o.Logger,
		tracer:   o.Tracer,
		turns:    turns,
		repairs:  repairs,
		supports: o.Supports,
		store:    o.Store,
		live:     map[acp.SessionId]*session{},
		busy:     map[acp.SessionId]*idLock{},
	}}
	h.settings.Store(&settings{registry: registry, session: o.Session})
	return h, nil
}

// settings are what the configuration gives the host besides Open: the
// registry with the mode option, override and command, and the session
// section.
type settings struct {
	registry *Registry
	session  config.Session
}

// newRegistry is a copy of base, which may be nil, with what the host adds:
// the mode option starting in the configured mode, its override and
// command, and the context overrides when the turn takes them.
func newRegistry(base *Registry, cfg config.Session, supports Supports) (*Registry, error) {
	registry := &Registry{}
	if base != nil {
		registry = base.clone()
	}
	if err := errors.Join(
		registry.AddConfigOption(modeOption(acp.SessionConfigValueId(cfg.Mode))),
		registry.AddOverride(modeOverride()),
		registry.AddCommand(modeCommand()),
	); err != nil {
		return nil, err
	}
	if supports.ContextOverrides {
		if err := errors.Join(
			registry.AddOverride(pathsOverride("context_files", func(turn *loop.Turn, paths []string) { turn.ContextFiles = paths })),
			registry.AddOverride(pathsOverride("skills_dirs", func(turn *loop.Turn, paths []string) { turn.SkillsDirs = paths })),
		); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (h *shared) registry() *Registry { return h.settings.Load().registry }

// Reload replaces the registry and the session section with those of a new
// configuration. A live session keeps each config value the new registry
// still accepts, and takes the option's new default for one it does not,
// such as a model that is no longer configured. Its client is sent the
// options and commands as they now are. The change applies from each
// session's next turn: the runtime reads the configuration per turn.
func (h *Host) Reload(ctx context.Context, base *Registry, cfg config.Session) error {
	registry, err := newRegistry(base, cfg, h.supports)
	if err != nil {
		return err
	}
	h.settings.Store(&settings{registry: registry, session: cfg})
	h.liveMu.Lock()
	type owned struct {
		s     *session
		owner *Agent
	}
	var live []owned
	for _, s := range h.live {
		live = append(live, owned{s, s.owner})
	}
	h.liveMu.Unlock()
	var errs []error
	for _, o := range live {
		errs = append(errs, o.owner.reloaded(ctx, o.s, registry))
	}
	return errors.Join(errs...)
}

// NewAgent returns the agent for one connection. Call SetClient before the
// connection is served, and Close once it has ended.
func (h *Host) NewAgent() *Agent { return &Agent{shared: h.shared} }

// Close ends the host and releases every live session after cancelling and
// waiting for its turn. An in-flight Open is cancelled and waited for too.
func (h *Host) Close(ctx context.Context) error {
	h.liveMu.Lock()
	h.ending = true
	h.stop()
	var sessions []*session
	for _, s := range h.live {
		sessions = append(sessions, s)
	}
	h.liveMu.Unlock()
	var errs []error
	for _, s := range sessions {
		s.beginClose(ctx)
	}
	for _, s := range sessions {
		errs = append(errs, s.close(ctx))
		if s.finished() {
			h.forget(s)
		}
	}
	h.opening.Wait()
	return errors.Join(errs...)
}

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
	for _, s := range a.live {
		if s.owner == a {
			owned = append(owned, s)
		}
	}
	a.liveMu.Unlock()
	var errs []error
	for _, s := range owned {
		s.beginClose(ctx)
	}
	for _, s := range owned {
		errs = append(errs, s.close(ctx))
		if s.finished() {
			a.forget(s)
		}
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

// forget removes only this instance, never a replacement attached meanwhile.
// The caller has already waited for its turn and runtime to stop.
func (h *shared) forget(s *session) {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	if h.live[s.id] == s {
		delete(h.live, s.id)
	}
}

// drain leaves a closing session visible even when ctx stops waiting, so
// a later attach must still wait for its turn and runtime to finish. A close
// that finished with an error is over: the session is forgotten and the
// error returned. Callers hold the session's id lock.
func (h *shared) drain(ctx context.Context, id acp.SessionId, owner *Agent) (*session, error) {
	s, actual := h.lookup(id)
	if s == nil || owner != nil && actual != owner {
		return nil, nil
	}
	err := s.close(ctx)
	if s.finished() {
		h.forget(s)
	}
	return s, err
}

// put makes a session live on owner. It fails for an agent whose
// connection has ended, which would leave a session nobody can reach.
func (h *shared) put(s *session, owner *Agent) error {
	h.liveMu.Lock()
	defer h.liveMu.Unlock()
	if owner.ended || h.ending {
		return errors.New("session: the connection has ended")
	}
	if h.live[s.id] != nil {
		return errors.New("session: a previous owner has not finished draining")
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
