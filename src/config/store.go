package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Store configures where sessions are kept. src/store/dir reads it.
type Store struct {
	// Root is the directory that holds one directory per session. It is an
	// absolute path. Empty means the XDG state directory: Dir resolves it.
	Root string `name:"root" placeholder:"DIR" help:"Absolute directory that holds one directory per session. Empty means towa/sessions in the XDG state directory."`
	// Compression is how the log of a new session is stored. A session
	// that exists keeps the format it was created with.
	Compression string `name:"compression" default:"zstd" validate:"oneof=none zstd" help:"How the log of a new session is stored: none or zstd."`
	// Fsync flushes every write of a session to the disk before the write
	// is acknowledged. Off, a crash of the machine can lose or tear the
	// last writes, and the next open drops a torn record.
	Fsync bool `name:"fsync" default:"true" help:"Flush every write of a session to the disk before it is acknowledged. On by default. Off is faster, and a crash of the machine can lose the last writes."`
}

// Values of Store.Compression. They are the names src/store/dir takes.
const (
	// StoreCompressionNone keeps the log as JSON lines.
	StoreCompressionNone = "none"
	// StoreCompressionZstd keeps the log as one zstd frame per append.
	StoreCompressionZstd = "zstd"
)

func (s Store) validate() error {
	if s.Root != "" && !filepath.IsAbs(s.Root) {
		return fmt.Errorf("store.root: %q is not an absolute path", s.Root)
	}
	return validateFields("store", s)
}

// Dir returns the directory sessions are kept in. When Root is empty it is
// towa/sessions under XDG_STATE_HOME, or under ~/.local/state when
// that is not set or not absolute, as the XDG Base Directory specification
// says. environ is the environment, as os.Environ returns it.
func (s Store) Dir(environ []string) (string, error) {
	if s.Root != "" {
		return s.Root, nil
	}
	state := storeEnv(environ, "XDG_STATE_HOME")
	if !filepath.IsAbs(state) {
		home := storeEnv(environ, "HOME")
		if !filepath.IsAbs(home) {
			return "", errors.New("store.root is not set, and neither XDG_STATE_HOME nor HOME is an absolute path to derive it from")
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "towa", "sessions"), nil
}

// storeEnv returns the value of name in environ, or "" when it is not set.
// The last assignment wins, as it does for a process.
func storeEnv(environ []string, name string) string {
	value := ""
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok && k == name {
			value = v
		}
	}
	return value
}
