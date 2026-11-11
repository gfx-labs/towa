package auth

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotLoggedIn says the store holds no login for an OAuth account.
var ErrNotLoggedIn = errors.New("account has no stored login")

// Tokens is the stored login of one OAuth account: its entry in auth.json.
type Tokens struct {
	// Provider is the provider the login was made with. An entry is not used
	// for an account that the configuration gives to another provider.
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

// Store keeps every account's login in one file, auth.json, in a directory
// only the user can read. An account in the file needs no entry in the
// configuration: a login creates it.
type Store struct{ dir string }

// accountsFile is the content of auth.json.
type accountsFile struct {
	Accounts map[string]Tokens `json:"accounts"`
}

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

// plainName refuses a name that is not a plain file name. An account id
// names the account's lock file and a provider its pending login, and
// neither may reach outside the directory.
func plainName(what, name string) error {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return fmt.Errorf("%s %q cannot name a file in the auth store", what, name)
	}
	return nil
}

func (s *Store) accountsPath() string { return filepath.Join(s.dir, "auth.json") }

// read returns the content of auth.json. Without the file there are no
// accounts.
func (s *Store) read() (accountsFile, error) {
	var f accountsFile
	if err := readFile(s.accountsPath(), &f); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return accountsFile{}, err
	}
	return f, nil
}

// update rewrites auth.json with change applied. The file holds every
// account, so a writer takes one lock for its read and its write, and the
// refresh of one account does not undo the refresh of another.
func (s *Store) update(change func(accounts map[string]Tokens) error) (err error) {
	// No context: the lock is held for one read and one write, and a
	// refreshed token must be stored whatever became of its caller.
	unlock, err := lockFile(context.Background(), s.accountsPath()+".lock")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	f, err := s.read()
	if err != nil {
		return err
	}
	if f.Accounts == nil {
		f.Accounts = map[string]Tokens{}
	}
	if err := change(f.Accounts); err != nil {
		return err
	}
	return writeFile(s.accountsPath(), &f)
}

// Accounts returns the provider of every account in the store, by id.
func (s *Store) Accounts() (map[string]string, error) {
	f, err := s.read()
	if err != nil {
		return nil, err
	}
	providers := make(map[string]string, len(f.Accounts))
	for id, t := range f.Accounts {
		providers[id] = t.Provider
	}
	return providers, nil
}

// Load reads the account's login. Without one the error is ErrNotLoggedIn.
// It takes no lock: a writer replaces the file whole.
func (s *Store) Load(account string) (Tokens, error) {
	if err := plainName("account id", account); err != nil {
		return Tokens{}, err
	}
	f, err := s.read()
	if err != nil {
		return Tokens{}, err
	}
	t, ok := f.Accounts[account]
	if !ok {
		return Tokens{}, fmt.Errorf("account %q: %w", account, ErrNotLoggedIn)
	}
	return t, nil
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

// Save replaces the account's login, and makes the account when the store
// has none of that id. A reader sees the old file or the new one, never a
// part of either.
func (s *Store) Save(account string, t Tokens) error {
	if err := plainName("account id", account); err != nil {
		return err
	}
	return s.update(func(accounts map[string]Tokens) error {
		accounts[account] = t
		return nil
	})
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
	// Sorted and on several lines: the file is one a person may read.
	if err := json.MarshalWrite(tmp, v, json.Deterministic(true), jsontext.Multiline(true)); err != nil {
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

// Delete removes the account from the store. Without one the error is
// ErrNotLoggedIn.
func (s *Store) Delete(account string) error {
	if err := plainName("account id", account); err != nil {
		return err
	}
	return s.update(func(accounts map[string]Tokens) error {
		if _, ok := accounts[account]; !ok {
			return fmt.Errorf("account %q: %w", account, ErrNotLoggedIn)
		}
		delete(accounts, account)
		return nil
	})
}

// Lock takes the account's exclusive lock, waiting for whichever process or
// goroutine holds it. The returned function releases it.
//
// The lock is on a file of its own. Save replaces auth.json, and a lock on a
// replaced file excludes nobody. The lock file is never removed, for the
// same reason. It is one per account, so that the refresh of one account
// does not wait for the token request of another.
func (s *Store) Lock(ctx context.Context, account string) (unlock func() error, err error) {
	if err := plainName("account id", account); err != nil {
		return nil, err
	}
	// No id makes this the name of the lock update takes.
	return lockFile(ctx, filepath.Join(s.dir, "account."+account+".lock"))
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
