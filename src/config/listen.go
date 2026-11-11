package config

import (
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
)

// Listen configures where the agent takes ACP connections: standard input
// and output, a unix socket and a TCP address. Each connection has its own
// sessions.
type Listen struct {
	// Stdio serves one connection on standard input and output.
	Stdio bool `name:"stdio" default:"true" help:"Serve one connection on standard input and output. On by default."`
	// MaxConnections is the most socket connections served at once, over
	// both listeners. At the limit a connection waits until one ends.
	// Each listener may hold one accepted connection waiting for a slot,
	// with further connections queued in the kernel. Zero is no limit.
	MaxConnections int `name:"max_connections" default:"0" validate:"gte=0" help:"Most socket connections served at once, over both listeners. 0 is no limit."`
	// HandshakeTimeoutSeconds bounds a TLS handshake. Zero is no bound. The
	// default is not measured. It is the TLSHandshakeTimeout of net/http's
	// DefaultTransport, the bound Go's own client puts on the same step.
	HandshakeTimeoutSeconds int `name:"handshake_timeout_seconds" default:"10" validate:"gte=0" help:"Seconds a TLS handshake may take. 0 is no bound."`

	Unix ListenUnix `embed:"" prefix:"unix."`
	TCP  ListenTCP  `embed:"" prefix:"tcp."`
}

// ListenUnix configures the unix socket. It is on when Path is set.
type ListenUnix struct {
	// Path is the socket file. It is absolute.
	Path string `name:"path" placeholder:"FILE" help:"Absolute path of a unix socket to serve on. Empty means no unix socket."`
	// Mode is the socket file's permission bits in octal, such as "0600".
	// A process connects when it may write the file.
	Mode string    `name:"mode" default:"0600" help:"Permission bits of the socket file, in octal. In a file, write it in quotes."`
	TLS  ListenTLS `embed:"" prefix:"tls."`
}

// ListenTCP configures the TCP listener. It is on when Address is set.
type ListenTCP struct {
	// Address is host:port. An empty host is every interface.
	Address string `name:"address" placeholder:"HOST:PORT" help:"TCP address to serve on. An empty host is every interface. Empty means no TCP listener."`
	// PlaintextOnNonLoopback allows an address that is not a loopback one
	// to be served without TLS. Anyone who reaches the port then drives the
	// agent, so it is for a network that is already private.
	PlaintextOnNonLoopback bool      `name:"plaintext_on_non_loopback" help:"Serve an address that is not a loopback one without TLS. Anyone who reaches the port then drives the agent."`
	TLS                    ListenTLS `embed:"" prefix:"tls."`
}

// ListenTLS configures TLS on one listener. It is on when CertFile is set.
type ListenTLS struct {
	// CertFile and KeyFile are the PEM certificate chain and its key.
	CertFile string `name:"cert_file" placeholder:"FILE" validate:"required_with=KeyFile ClientCAFile" help:"PEM certificate chain. Setting it turns TLS on for this listener."`
	KeyFile  string `name:"key_file" placeholder:"FILE" validate:"required_with=CertFile" help:"PEM key of the certificate."`
	// ClientCAFile is a PEM file of the authorities that sign client
	// certificates. When it is set, a certificate a client presents must
	// verify against it.
	ClientCAFile string `name:"client_ca_file" placeholder:"FILE" validate:"required_if=RequireClientCert true" help:"PEM file of the authorities that sign client certificates. A certificate a client presents must verify against it."`
	// RequireClientCert refuses a client that presents no certificate.
	RequireClientCert bool `name:"require_client_cert" help:"Refuse a client that presents no certificate."`
}

// Enabled reports whether TLS is on.
func (t ListenTLS) Enabled() bool { return t.CertFile != "" }

// FileMode returns Mode as permission bits.
func (u ListenUnix) FileMode() (fs.FileMode, error) {
	bits, err := strconv.ParseUint(u.Mode, 8, 32)
	if err != nil || fs.FileMode(bits)&^fs.ModePerm != 0 {
		return 0, fmt.Errorf("listen.unix.mode: %q is not octal permission bits such as \"0600\"", u.Mode)
	}
	return fs.FileMode(bits), nil
}

// Loopback reports whether Address can only be reached from this machine.
// A host name other than localhost is not resolved and counts as reachable.
func (t ListenTCP) Loopback() bool {
	host, _, err := net.SplitHostPort(t.Address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func (l Listen) validate() error { return l.Validate() }

// Validate checks listener addresses, permissions and TLS settings without
// opening sockets or reading certificate files.
func (l Listen) Validate() error {
	if err := validateFields("listen", l); err != nil {
		return err
	}
	if !l.Stdio && l.Unix.Path == "" && l.TCP.Address == "" {
		return fmt.Errorf("listen: stdio is off and neither listen.unix.path nor listen.tcp.address is set, so nothing can connect")
	}
	if l.Unix.Path != "" {
		if _, err := l.Unix.FileMode(); err != nil {
			return err
		}
	}
	if l.Unix.Path == "" {
		if l.Unix.TLS.Enabled() {
			return fmt.Errorf("listen.unix.tls.cert_file is set and listen.unix.path is empty")
		}
	} else if !filepath.IsAbs(l.Unix.Path) {
		return fmt.Errorf("listen.unix.path: %q is not absolute", l.Unix.Path)
	}
	if l.TCP.Address == "" {
		switch {
		case l.TCP.TLS.Enabled():
			return fmt.Errorf("listen.tcp.tls.cert_file is set and listen.tcp.address is empty")
		case l.TCP.PlaintextOnNonLoopback:
			return fmt.Errorf("listen.tcp.plaintext_on_non_loopback is set and listen.tcp.address is empty")
		}
		return nil
	}
	if _, _, err := net.SplitHostPort(l.TCP.Address); err != nil {
		return fmt.Errorf("listen.tcp.address: %q is not host:port", l.TCP.Address)
	}
	if !l.TCP.TLS.Enabled() && !l.TCP.Loopback() && !l.TCP.PlaintextOnNonLoopback {
		return fmt.Errorf("listen.tcp.address: %q is not a loopback address and listen.tcp.tls.cert_file is empty: "+
			"configure TLS, or set listen.tcp.plaintext_on_non_loopback if an open port is intended", l.TCP.Address)
	}
	return nil
}
