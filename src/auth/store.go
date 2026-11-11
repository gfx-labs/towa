package auth

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotLoggedIn says an OAuth account has no file in the store.
var ErrNotLoggedIn = errors.New("account has no stored login")

// Tokens is the stored login of one OAuth account: the content of its file.
type Tokens struct {
	// Provider is the provider the login was made with. A file is not used
	// for an account that the configuration now gives to another provider.
	Provider     string `json:"provider"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	// ExpiresAtUnixMs is when the access token stops working.
	ExpiresAtUnixMs  int64    `json:"expires_at_unix_ms"`
	Scopes           []string `json:"scopes,omitzero"`
	ChatGPTAccountID string   `json:"chatgpt_account_id,omitzero"`
	// RefreshRejectedAtUnixMs is when the token endpoint said the refresh
	// token is dead, and RefreshRejection the code it gave. While they are
	// set the login is not used and no refresh is sent. A new login writes
	// the file without them.
	RefreshRejectedAtUnixMs int64  `json:"refresh_rejected_at_unix_ms,omitzero"`
	RefreshRejection        string `json:"refresh_rejection,omitzero"`
}

// ExpiresAt is when the access token stops working.
func (t Tokens) ExpiresAt() time.Time { return time.UnixMilli(t.ExpiresAtUnixMs) }

// Store keeps one file per account in a directory only the user can read.
type Store struct{ dir string }

// NewStore returns the store in dir. The directory is made on first write.
func NewStore(dir string) *Store { return &Store{dir: dir} }

// StateDir is the auth store's directory by the XDG rule, given the values
// of XDG_STATE_HOME and HOME.
func StateDir(xdgStateHome, home string) (string, error) {
	switch {
	case xdgStateHome != "":
		return filepath.Join(xdgStateHome, "acpharness", "auth"), nil
	case home != "":
		return filepath.Join(home, ".local", "state", "acpharness", "auth"), nil
	}
	return "", errors.New("neither XDG_STATE_HOME nor HOME is set, so there is no place for the auth store")
}

// path is the account's file with the given extension. An id that is not a
// plain file name would reach outside the directory.
func (s *Store) path(account, ext string) (string, error) {
	if account == "" || account != filepath.Base(account) || strings.HasPrefix(account, ".") {
		return "", fmt.Errorf("account id %q cannot name a file in the auth store", account)
	}
	return filepath.Join(s.dir, account+ext), nil
}

// Load reads the account's file. Without one the error is ErrNotLoggedIn.
func (s *Store) Load(account string) (Tokens, error) {
	path, err := s.path(account, ".json")
	if err != nil {
		return Tokens{}, err
	}
	var t Tokens
	err = readFile(path, &t)
	if errors.Is(err, fs.ErrNotExist) {
		return Tokens{}, fmt.Errorf("account %q: %w", account, ErrNotLoggedIn)
	}
	return t, err
}

// readFile decodes the token file at path into v. A missing file is
// fs.ErrNotExist.
func readFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // read only
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o: a token file must be readable by its owner only", path, perm)
	}
	if err := json.UnmarshalRead(f, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// Save replaces the account's file. A reader sees the old file or the new
// one, never a part of either.
func (s *Store) Save(account string, t Tokens) error {
	path, err := s.path(account, ".json")
	if err != nil {
		return err
	}
	return writeFile(path, &t)
}

// writeFile replaces the token file at path with v, by a temp file in the
// same directory and a rename.
func writeFile(path string, v any) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// CreateTemp makes the file with mode 0600.
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tmp.Close(), os.Remove(tmp.Name()))
		}
	}()
	if err := json.MarshalWrite(tmp, v); err != nil {
		return err
	}
	// Without the sync a crash after the rename can leave an empty file.
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(err, os.Remove(tmp.Name()))
	}
	return nil
}

// Delete removes the account's file. Without one the error is ErrNotLoggedIn.
func (s *Store) Delete(account string) error {
	path, err := s.path(account, ".json")
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("account %q: %w", account, ErrNotLoggedIn)
	}
	return err
}

// Lock takes the account's exclusive lock, waiting for whichever process or
// goroutine holds it. The returned function releases it.
//
// The lock is on a file of its own beside the account's file. Save replaces
// the account's file, and a lock on a replaced file excludes nobody. The lock
// file is never removed, for the same reason.
func (s *Store) Lock(ctx context.Context, account string) (unlock func() error, err error) {
	path, err := s.path(account, ".lock")
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, path)
}

// lockFile takes the exclusive lock on the file at path, making it first.
func lockFile(ctx context.Context, path string) (unlock func() error, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	// flock cannot be interrupted, so it waits in a goroutine.
	locked := make(chan error, 1)
	go func() { locked <- flockExclusive(f) }()
	select {
	case err := <-locked:
		if err != nil {
			return nil, errors.Join(fmt.Errorf("lock %s: %w", path, err), f.Close())
		}
		// Closing the only descriptor of the open file releases the lock.
		return f.Close, nil
	case <-ctx.Done():
		// Nobody is left to report to. The close releases a lock that was
		// granted after the caller gave up.
		go func() { _ = errors.Join(<-locked, f.Close()) }()
		return nil, context.Cause(ctx)
	}
}
