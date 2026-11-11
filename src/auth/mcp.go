package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// MCPTokens is the stored OAuth login to one MCP server. The MCP client
// makes and refreshes it. The store only keeps it.
type MCPTokens struct {
	// ServerURL is the server the tokens were issued for. The caller
	// compares it with the server's configured URL, so that tokens are not
	// sent to a server name that now points elsewhere.
	ServerURL    string `json:"server_url"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitzero"`
	TokenType    string `json:"token_type,omitzero"`
	// ExpiresAtUnixMs is zero when the server gave no expiry.
	ExpiresAtUnixMs int64    `json:"expires_at_unix_ms,omitzero"`
	Scopes          []string `json:"scopes,omitzero"`
	// TokenEndpoint is where the refresh grant is sent. It is known only
	// at the code exchange, and a later process would otherwise have to
	// discover the authorization server again.
	TokenEndpoint string `json:"token_endpoint,omitzero"`
	// ClientID and ClientSecret are the client the tokens belong to, which
	// dynamic registration may have made at login.
	ClientID     string `json:"client_id,omitzero"`
	ClientSecret string `json:"client_secret,omitzero"`
}

// mcpFile is the content of one MCP server's file.
type mcpFile struct {
	// Server is the name the file is for. The file name is a hash of it.
	Server string    `json:"server"`
	Tokens MCPTokens `json:"tokens"`
}

// mcpPath is the server's file with the given extension, in the store's mcp
// directory. A client names the servers of its session, so a name may hold
// a slash, a space or a leading dot. The file is named by the SHA-256 of
// the name, which is a plain file name for any name and differs for names
// that differ only in case.
func (s *Store) mcpPath(server, ext string) (string, error) {
	if server == "" {
		return "", errors.New("an MCP server without a name has no file in the auth store")
	}
	sum := sha256.Sum256([]byte(server))
	return filepath.Join(s.dir, "mcp", hex.EncodeToString(sum[:])+ext), nil
}

// LoadMCP reads the server's tokens. Without a file the error is
// ErrNotLoggedIn.
func (s *Store) LoadMCP(server string) (MCPTokens, error) {
	path, err := s.mcpPath(server, ".json")
	if err != nil {
		return MCPTokens{}, err
	}
	var f mcpFile
	err = readFile(path, &f)
	if errors.Is(err, fs.ErrNotExist) {
		return MCPTokens{}, fmt.Errorf("MCP server %q: %w", server, ErrNotLoggedIn)
	}
	if err != nil {
		return MCPTokens{}, err
	}
	if f.Server != server {
		return MCPTokens{}, fmt.Errorf("%s holds the login of MCP server %q, not of %q", path, f.Server, server)
	}
	return f.Tokens, nil
}

// SaveMCP replaces the server's tokens. A reader sees the old file or the
// new one, never a part of either.
func (s *Store) SaveMCP(server string, t MCPTokens) error {
	path, err := s.mcpPath(server, ".json")
	if err != nil {
		return err
	}
	return writeFile(path, &mcpFile{Server: server, Tokens: t})
}

// DeleteMCP removes the server's tokens. Without a file the error is
// ErrNotLoggedIn.
func (s *Store) DeleteMCP(server string) error {
	path, err := s.mcpPath(server, ".json")
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("MCP server %q: %w", server, ErrNotLoggedIn)
	}
	return err
}

// LockMCP takes the server's exclusive lock, as Lock does for an account.
// The holder re-reads the tokens before it refreshes them.
func (s *Store) LockMCP(ctx context.Context, server string) (unlock func() error, err error) {
	path, err := s.mcpPath(server, ".lock")
	if err != nil {
		return nil, err
	}
	return lockFile(ctx, path)
}
