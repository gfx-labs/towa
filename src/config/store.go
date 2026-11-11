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
	Root string `koanf:"root"`
	// Compression is how the log of a new session is stored. A session
	// that exists keeps the format it was created with.
	Compression string `koanf:"compression" validate:"oneof=none zstd"`
}

// Values of Store.Compression. They are the names src/store/dir takes.
const (
	// StoreCompressionNone keeps the log as JSON lines.
	StoreCompressionNone = "none"
	// StoreCompressionZstd keeps the log as one zstd frame per append.
	StoreCompressionZstd = "zstd"
)

// The measurements behind the compression default are in
// docs/notes/2026-11-11-store.md.
var storeDefaults = map[string]any{
	"store.root":        "",
	"store.compression": StoreCompressionZstd,
}

func (s Store) validate() error {
	if s.Root != "" && !filepath.IsAbs(s.Root) {
		return fmt.Errorf("store.root: %q is not an absolute path", s.Root)
	}
	return validateFields("store", s)
}

// Dir returns the directory sessions are kept in. When Root is empty it is
// acpharness/sessions under XDG_STATE_HOME, or under ~/.local/state when
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
	return filepath.Join(state, "acpharness", "sessions"), nil
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
