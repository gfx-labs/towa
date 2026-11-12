package auth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ErrNoPendingLogin says no login of the provider waits for its code.
var ErrNoPendingLogin = errors.New("no login is waiting for a code")

// PendingLogin is a login that showed its page in one run of the command and
// takes the code in a later run. It names the account, so the later run
// needs only the provider and the code.
type PendingLogin struct {
	Account     string `json:"account"`
	RedirectURI string `json:"redirect_uri"`
	Verifier    string `json:"verifier"`
	State       string `json:"state"`
}

// Pending is what a later process needs to finish the login for account.
func (l *Login) Pending(account string) PendingLogin {
	return PendingLogin{Account: account, RedirectURI: l.redirectURI, Verifier: l.verifier, State: l.state}
}

// Resume continues a login that an earlier process began.
func (f Flow) Resume(p PendingLogin) *Login {
	return &Login{flow: f, redirectURI: p.RedirectURI, verifier: p.Verifier, state: p.State}
}

// pendingPath is the file of the provider's pending login. There is one per
// provider: a new login replaces the one that was waiting.
func (s *Store) pendingPath(provider string) (string, error) {
	if err := plainName("provider", provider); err != nil {
		return "", err
	}
	return filepath.Join(s.dir, "pending", provider+".json"), nil
}

// SavePending records the login that waits for the provider's code.
func (s *Store) SavePending(ctx context.Context, provider string, p PendingLogin) (err error) {
	path, err := s.pendingPath(provider)
	if err != nil {
		return err
	}
	unlock, err := lockFile(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	return writeFile(path, &p)
}

// LoadPending reads the login that waits for the provider's code. Without
// one the error is ErrNoPendingLogin.
func (s *Store) LoadPending(provider string) (PendingLogin, error) {
	path, err := s.pendingPath(provider)
	if err != nil {
		return PendingLogin{}, err
	}
	var p PendingLogin
	err = readFile(path, &p)
	if errors.Is(err, fs.ErrNotExist) {
		return PendingLogin{}, fmt.Errorf("%s: %w", provider, ErrNoPendingLogin)
	}
	return p, err
}

// DeletePending removes only the login that the caller completed. A newer
// pending login stays. Without one the error is ErrNoPendingLogin.
func (s *Store) DeletePending(ctx context.Context, provider string, completed PendingLogin) (err error) {
	path, err := s.pendingPath(provider)
	if err != nil {
		return err
	}
	// SavePending takes the same lock, so a replacement cannot race the removal.
	unlock, err := lockFile(ctx, path+".lock")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	current, err := s.LoadPending(provider)
	if err != nil {
		return err
	}
	if current != completed {
		return nil
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", provider, ErrNoPendingLogin)
	}
	return err
}
