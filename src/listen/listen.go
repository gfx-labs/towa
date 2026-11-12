// Package listen opens the unix socket and TCP listeners and hands each
// connection to the caller. A connection carries the same newline-delimited
// JSON-RPC as standard input and output. The package knows nothing of
// sessions.
package listen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/gfx-labs/towa/src/config"
	"github.com/gfx-labs/towa/src/filelock"
)

// Handler serves one connection and returns when the client hangs up or ctx
// ends. ctx is cancelled on shutdown. The connection is closed after Handler
// returns, so Handler need not close it. On a TLS listener conn is a
// *tls.Conn whose handshake is done.
type Handler func(ctx context.Context, conn net.Conn) error

// ErrInUse reports a unix socket path that a live process serves.
var ErrInUse = errors.New("listen: the unix socket is in use by a live process")

// Listeners are the open listeners of one process.
type Listeners struct {
	logger *slog.Logger
	open   []listener
	lock   *os.File
	// socket is the socket file once this process has bound it.
	socket    string
	slots     chan struct{}
	handshake time.Duration
}

type listener struct {
	net.Listener
	tls *tls.Config
}

// Open opens the listeners cfg names: the unix socket when its path is set
// and TCP when its address is set. It reads the certificate files once, so
// that a bad file fails here. A nil logger discards log records. The caller
// calls Serve, or Close if it does not.
func Open(ctx context.Context, cfg config.Listen, logger *slog.Logger) (*Listeners, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Unix.Path == "" && cfg.TCP.Address == "" {
		return nil, errors.New("listen: neither listen.unix.path nor listen.tcp.address is set")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	l := &Listeners{
		logger:    logger,
		handshake: time.Duration(cfg.HandshakeTimeoutSeconds) * time.Second,
	}
	if cfg.MaxConnections > 0 {
		l.slots = make(chan struct{}, cfg.MaxConnections)
	}
	if err := l.openAll(ctx, cfg); err != nil {
		return nil, errors.Join(err, l.Close())
	}
	return l, nil
}

func (l *Listeners) openAll(ctx context.Context, cfg config.Listen) error {
	if cfg.Unix.Path != "" {
		tlsConfig, err := serverTLS(cfg.Unix.TLS)
		if err != nil {
			return fmt.Errorf("listen: unix: %w", err)
		}
		ln, err := l.openUnix(ctx, cfg.Unix)
		if err != nil {
			return err
		}
		l.open = append(l.open, listener{Listener: ln, tls: tlsConfig})
	}
	if cfg.TCP.Address != "" {
		tlsConfig, err := serverTLS(cfg.TCP.TLS)
		if err != nil {
			return fmt.Errorf("listen: tcp: %w", err)
		}
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.TCP.Address)
		if err != nil {
			return fmt.Errorf("listen: tcp: %w", err)
		}
		l.open = append(l.open, listener{Listener: ln, tls: tlsConfig})
	}
	return nil
}

// openUnix binds the socket file. A file left by a dead process is in the
// way of the bind, and is removed only when it is a socket that refuses a
// connection. The lock beside it is held until Close, so that two processes
// starting together cannot each take the other's new socket for a stale one.
func (l *Listeners) openUnix(ctx context.Context, cfg config.ListenUnix) (net.Listener, error) {
	mode, err := cfg.FileMode()
	if err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(cfg.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("listen: unix: %w", err)
	}
	l.lock = lock
	if err := filelock.TryLock(lock, filelock.Exclusive); err != nil {
		if errors.Is(err, filelock.ErrHeld) {
			return nil, fmt.Errorf("%w: %s (its lock %s is held)", ErrInUse, cfg.Path, lock.Name())
		}
		return nil, fmt.Errorf("listen: unix: lock %s: %w", lock.Name(), err)
	}
	if err := removeStale(ctx, cfg.Path); err != nil {
		return nil, err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "unix", cfg.Path)
	if err != nil {
		return nil, fmt.Errorf("listen: unix: %w", err)
	}
	l.socket = cfg.Path
	// Close removes the file itself, after the connections have ended.
	if u, ok := ln.(*net.UnixListener); ok {
		u.SetUnlinkOnClose(false)
	}
	// The file has the mode the umask gave it until here. A connection made
	// in between is in the queue and would be served, so the mode is set
	// before the first Accept and the directory is what keeps others out.
	if err := os.Chmod(cfg.Path, mode); err != nil {
		l.open = append(l.open, listener{Listener: ln})
		return nil, fmt.Errorf("listen: unix: %w", err)
	}
	return ln, nil
}

func removeStale(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listen: unix: %w", err)
	}
	if info.Mode().Type() != fs.ModeSocket {
		return fmt.Errorf("listen: unix: %s exists and is not a socket", path)
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err == nil {
		return errors.Join(fmt.Errorf("%w: %s", ErrInUse, path), conn.Close())
	}
	// Only a refusal shows that nothing listens. Any other failure, such as
	// a full queue or no permission, leaves the file alone.
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("listen: unix: cannot tell whether %s is in use: %w", path, err)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("listen: unix: remove the stale socket: %w", err)
	}
	return nil
}

// serverTLS returns nil when TLS is off. The returned config reads the files
// again for every handshake, so a rotated certificate or client CA applies
// to the next connection. A connection lasts as long as its client, so the
// read is rare.
func serverTLS(cfg config.ListenTLS) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	if _, err := loadTLS(cfg); err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return loadTLS(cfg) },
	}, nil
}

func loadTLS(cfg config.ListenTLS) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls certificate: %w", err)
	}
	out := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}}
	if cfg.ClientCAFile == "" {
		return out, nil
	}
	pem, err := os.ReadFile(cfg.ClientCAFile)
	if err != nil {
		return nil, fmt.Errorf("tls client CA: %w", err)
	}
	out.ClientCAs = x509.NewCertPool()
	if !out.ClientCAs.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls client CA: %s holds no certificate", cfg.ClientCAFile)
	}
	out.ClientAuth = tls.VerifyClientCertIfGiven
	if cfg.RequireClientCert {
		out.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return out, nil
}

// Addrs returns the listeners' addresses, the unix socket before TCP.
func (l *Listeners) Addrs() []net.Addr {
	addrs := make([]net.Addr, 0, len(l.open))
	for _, o := range l.open {
		addrs = append(addrs, o.Addr())
	}
	return addrs
}

// stopAccepting closes the listeners and leaves the socket file in place. A
// client that connects from here on is refused.
func (l *Listeners) stopAccepting() error {
	var errs []error
	for _, o := range l.open {
		if err := o.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close closes the listeners, removes the socket file and gives up the
// lock. The lock file stays: removing it would let a process that already
// opened it hold a lock nobody else sees. Serve calls Close.
func (l *Listeners) Close() error {
	var errs []error
	if err := l.stopAccepting(); err != nil {
		errs = append(errs, err)
	}
	if l.socket != "" {
		if err := os.Remove(l.socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
		l.socket = ""
	}
	if l.lock != nil {
		if err := l.lock.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Serve accepts connections and runs handle for each in its own goroutine,
// until ctx ends or an Accept fails. Then it stops accepting, cancels the
// handlers' contexts, closes their connections to unblock reads and writes,
// waits for them and removes the socket file. A shutdown requested by ctx
// returns nil.
//
// An error from handle, or a failed handshake, is logged and ends only that
// connection.
func (l *Listeners) Serve(ctx context.Context, handle Handler) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	conns := &connSet{open: map[net.Conn]struct{}{}}
	var handlers, accepting sync.WaitGroup
	failed := make(chan error, len(l.open))
	for _, o := range l.open {
		accepting.Go(func() {
			if err := l.accept(ctx, o, handle, &handlers, conns); err != nil {
				failed <- err
			}
		})
	}

	var err error
	select {
	case <-ctx.Done():
	case err = <-failed:
	}
	cancel()
	stopErr := l.stopAccepting()
	// Every handler is started by an accept loop, so they are all counted
	// once the loops have ended.
	accepting.Wait()

	conns.closeAll()
	handlers.Wait()
	return errors.Join(err, stopErr, l.Close())
}

func (l *Listeners) accept(ctx context.Context, o listener, handle Handler, handlers *sync.WaitGroup, conns *connSet) error {
	for {
		conn, err := o.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("listen: accept on %s: %w", o.Addr(), err)
		}
		if l.slots != nil {
			select {
			case l.slots <- struct{}{}:
			case <-ctx.Done():
				_ = conn.Close()
				return nil
			}
		}
		conns.add(conn)
		handlers.Go(func() {
			l.serveConn(ctx, o, conn, handle, conns)
			if l.slots != nil {
				<-l.slots
			}
		})
	}
}

func (l *Listeners) serveConn(ctx context.Context, o listener, raw net.Conn, handle Handler, conns *connSet) {
	defer conns.remove(raw)
	logger := l.logger.With("listener", o.Addr().String(), "peer", raw.RemoteAddr().String())

	conn := raw
	if o.tls != nil {
		secured := tls.Server(raw, o.tls)
		conn = secured
		if err := l.handshakeTLS(ctx, secured); err != nil {
			logger.WarnContext(ctx, "listen: TLS handshake failed", "error", err)
			_ = raw.Close()
			return
		}
	}
	logger.DebugContext(ctx, "listen: connection opened")
	err := handle(ctx, conn)
	// A close that fails here is a peer that has already gone.
	_ = conn.Close()
	if err != nil && ctx.Err() == nil {
		logger.WarnContext(ctx, "listen: connection ended with an error", "error", err)
		return
	}
	logger.DebugContext(ctx, "listen: connection closed")
}

func (l *Listeners) handshakeTLS(ctx context.Context, conn *tls.Conn) error {
	if l.handshake > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, l.handshake)
		defer cancel()
	}
	return conn.HandshakeContext(ctx)
}

// connSet holds the connections to close when shutdown cancels their handlers.
type connSet struct {
	mu   sync.Mutex
	open map[net.Conn]struct{}
}

func (s *connSet) add(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open[c] = struct{}{}
}

func (s *connSet) remove(c net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, c)
}

func (s *connSet) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.open {
		_ = c.Close()
	}
}
