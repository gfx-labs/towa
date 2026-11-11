package listen_test

import (
	"net"
	"testing"

	"github.com/stretchr/testify/require"
)

// An idle Accept on one listener must not reserve the only slot and prevent
// the other listener from serving. The configured cap is process-wide.
func TestConnectionCapAcrossUnixAndTCP(t *testing.T) {
	cfg := unixConfig(t)
	cfg.MaxConnections = 1
	cfg.TCP.Address = "127.0.0.1:0"
	s := serve(t, cfg)

	for _, addr := range s.listeners.Addrs() {
		conn, err := net.DialTimeout(addr.Network(), addr.String(), timeout)
		require.NoError(t, err)
		got, err := newClient(t, conn).ask(addr.Network())
		require.NoError(t, err)
		require.Equal(t, addr.Network(), got)
		require.NoError(t, conn.Close())
	}
}
