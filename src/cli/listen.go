package cli

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"

	"github.com/gfx-labs/towa/src/agent/session"
	"github.com/gfx-labs/towa/src/cli/listen"
	"github.com/gfx-labs/towa/src/config"
)

// serveListeners serves every configured transport on host. The caller closes
// host after this returns. In socket mode, stdio EOF or failure retires only
// stdio. Without sockets, stdio's result ends the invocation.
//
// closeStdio is an explicit ownership handoff: when non-nil it closes the
// invocation's stdio streams on shutdown, unblocking reads and writes. It is
// never inferred from h or from global file descriptors. With nil, the caller
// must ensure stdio writes unblock on cancellation. A blocked stdio reader
// can outlive ACP Serve until the reader ends or the process exits.
func serveListeners(ctx context.Context, cfg config.Listen, host *session.Host, h Host, logger *slog.Logger, closeStdio func() error) (result error) {
	if err := cfg.Validate(); err != nil {
		return err
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	sockets := cfg.Unix.Path != "" || cfg.TCP.Address != ""
	var listeners *listen.Listeners
	if sockets {
		var err error
		listeners, err = listen.Open(ctx, cfg, logger)
		if err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Stdio && closeStdio != nil {
		closed := make(chan error, 1)
		go func() {
			<-ctx.Done()
			closed <- closeStdio()
		}()
		defer func() {
			cancel()
			result = errors.Join(result, <-closed)
		}()
	}

	var stdioDone, socketsDone chan error
	pending := 0
	if sockets {
		socketsDone = make(chan error, 1)
		pending++
		for _, addr := range listeners.Addrs() {
			logger.InfoContext(ctx, "serving ACP on socket", "network", addr.Network(), "address", addr.String())
		}
		go func() {
			socketsDone <- listeners.Serve(ctx, func(ctx context.Context, conn net.Conn) error {
				return host.ServeConn(ctx, conn)
			})
		}()
	}
	if cfg.Stdio {
		stdioDone = make(chan error, 1)
		pending++
		logger.InfoContext(ctx, "serving ACP on stdio")
		go func() { stdioDone <- host.ServeConn(ctx, stdioStream{Reader: h.Stdin, Writer: h.Stdout}) }()
	}

	stopping := ctx.Done()
	for pending > 0 {
		select {
		case <-stopping:
			cancel()
			stopping = nil
		case err := <-stdioDone:
			stdioDone = nil
			pending--
			if sockets {
				if err != nil && ctx.Err() == nil {
					logger.WarnContext(ctx, "stdio connection ended with an error", "error", err)
				}
				continue
			}
			stopped := ctx.Err() != nil && (errors.Is(err, context.Cause(ctx)) ||
				errors.Is(err, net.ErrClosed) || errors.Is(err, fs.ErrClosed) || errors.Is(err, io.ErrClosedPipe))
			if err != nil && !stopped {
				result = errors.Join(result, err)
			}
			cancel()
			stopping = nil
		case err := <-socketsDone:
			socketsDone = nil
			pending--
			result = errors.Join(result, err)
			cancel()
			stopping = nil
		}
	}
	return result
}

type stdioStream struct {
	io.Reader
	io.Writer
}
