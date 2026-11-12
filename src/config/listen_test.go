package config_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gfx-labs/acpharness/src/config"
)

func TestListenRefusesAnOpenPortThatWasNotAskedFor(t *testing.T) {
	tls := []string{"listen.tcp.tls.cert_file=/etc/acpharness/cert.pem", "listen.tcp.tls.key_file=/etc/acpharness/key.pem"}
	tests := []struct {
		name string
		// set holds key=value pairs, each given as the flag --key=value.
		set []string
		// want is the key the error names. Empty means the config loads.
		want string
	}{
		{"loopback address in the clear", []string{"listen.tcp.address=127.0.0.1:7000"}, ""},
		{"IPv6 loopback in the clear", []string{"listen.tcp.address=[::1]:7000"}, ""},
		{"localhost in the clear", []string{"listen.tcp.address=localhost:7000"}, ""},
		{"every interface in the clear", []string{"listen.tcp.address=:7000"}, "listen.tcp.address"},
		{"0.0.0.0 in the clear", []string{"listen.tcp.address=0.0.0.0:7000"}, "listen.tcp.address"},
		{"a LAN address in the clear", []string{"listen.tcp.address=192.168.1.5:7000"}, "listen.tcp.address"},
		{"a host name in the clear", []string{"listen.tcp.address=agents.example:7000"}, "listen.tcp.address"},
		{"every interface in the clear, said plainly", []string{"listen.tcp.address=:7000", "listen.tcp.plaintext_on_non_loopback=true"}, ""},
		{"every interface with TLS", append([]string{"listen.tcp.address=:7000"}, tls...), ""},
		{"an address without a port", []string{"listen.tcp.address=127.0.0.1"}, "listen.tcp.address"},
		{"a certificate without a key", []string{"listen.tcp.address=:7000", tls[0]}, "listen.tcp.tls.key_file"},
		{"TLS without an address", tls, "listen.tcp.address"},
		{"a required client certificate with no authority", append([]string{"listen.tcp.address=:7000", "listen.tcp.tls.require_client_cert=true"}, tls...), "listen.tcp.tls.client_ca_file"},
		{"a relative socket path", []string{"listen.unix.path=acp.sock"}, "listen.unix.path"},
		{"a socket mode that is not octal", []string{"listen.unix.path=/run/acp.sock", "listen.unix.mode=rw"}, "listen.unix.mode"},
		{"a socket mode with more than permission bits", []string{"listen.unix.path=/run/acp.sock", "listen.unix.mode=4755"}, "listen.unix.mode"},
		{"nothing to connect to", []string{"listen.stdio=false"}, "listen"},
		{"a negative connection limit", []string{"listen.max_connections=-1"}, "listen.max_connections"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var args []string
			for _, kv := range tt.set {
				args = append(args, "--"+kv)
			}
			_, err := config.Load(config.Sources{}, args...)
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestPprofIsServedOnlyOnALoopbackAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:6060", "localhost:0", "[::1]:6060"} {
		loaded, err := config.Load(config.Sources{}, "--telemetry.pprof.address", address)
		require.NoError(t, err, address)
		require.Equal(t, address, loaded.Config.Telemetry.Pprof.Address)
	}
	for _, address := range []string{":6060", "0.0.0.0:6060", "example.com:6060", "127.0.0.1"} {
		_, err := config.Load(config.Sources{}, "--telemetry.pprof.address", address)
		require.ErrorContains(t, err, "telemetry.pprof.address", address)
	}
}
