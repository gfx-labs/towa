package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
)

// servePprof serves Go's profiling pages under /debug/pprof/ on address
// until the returned function is called. The pages show the program's
// memory and the command it was started with, and they have no
// authentication, which is why config accepts only a loopback address.
func servePprof(ctx context.Context, address string, logger *slog.Logger) (stop func() error, err error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("telemetry.pprof.address: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	// A profile takes as long as its caller asks, so the server sets no
	// timeouts. Only this machine reaches it.
	server := &http.Server{Handler: mux} //nolint:gosec // loopback only, see above
	done := make(chan error, 1)
	go func() { done <- server.Serve(ln) }()
	logger.Info("pprof", "address", ln.Addr().String())
	return func() error {
		err := server.Close()
		if served := <-done; !errors.Is(served, http.ErrServerClosed) {
			err = errors.Join(err, served)
		}
		return err
	}, nil
}
