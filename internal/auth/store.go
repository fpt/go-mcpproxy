// Package auth stores OAuth credentials for remote MCP servers and runs the
// interactive authorization flow.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
)

// Credentials are the persisted OAuth client registration and token for one
// server URL.
type Credentials struct {
	ServerURL    string           `json:"server_url"`
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	RedirectURI  string           `json:"redirect_uri,omitempty"`
	Scopes       []string         `json:"scopes,omitempty"`
	Token        *transport.Token `json:"token,omitempty"`
}

// Store keeps credentials as one 0600 JSON file per server URL in Dir.
// It is safe for concurrent use within a process.
type Store struct {
	Dir string
	mu  sync.Mutex
}

// NewStore returns a store rooted at dir.
func NewStore(dir string) *Store {
	return &Store{Dir: dir}
}

// key normalizes a server URL so that equivalent spellings share
// credentials: scheme and host are lowercased, and the fragment and a
// trailing slash are dropped.
func key(serverURL string) string {
	u, err := url.Parse(strings.TrimSpace(serverURL))
	if err != nil {
		return serverURL
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Fragment = ""
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

func (s *Store) path(serverURL string) string {
	sum := sha256.Sum256([]byte(key(serverURL)))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:8])+".json")
}

// Load returns the credentials for serverURL, or nil if there are none.
func (s *Store) Load(serverURL string) (*Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(serverURL)
}

func (s *Store) load(serverURL string) (*Credentials, error) {
	data, err := os.ReadFile(s.path(serverURL))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse credentials for %s: %w", serverURL, err)
	}
	if key(c.ServerURL) != key(serverURL) {
		// Hash collision or a hand-edited file; never hand out another
		// server's token.
		return nil, nil
	}
	return &c, nil
}

// Save writes credentials atomically with 0600 permissions.
func (s *Store) Save(c *Credentials) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(c)
}

func (s *Store) save(c *Credentials) error {
	c.ServerURL = key(c.ServerURL)
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	// Persisting the client secret is the point; the file is 0600.
	data, err := json.MarshalIndent(c, "", "  ") //nolint:gosec
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	tmp, err := os.CreateTemp(s.Dir, ".cred-*.json")
	if err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path(c.ServerURL)); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// Delete removes the credentials for serverURL. It reports whether any
// existed.
func (s *Store) Delete(serverURL string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.load(serverURL)
	if err != nil || c == nil {
		return false, err
	}
	if err := os.Remove(s.path(serverURL)); err != nil {
		return false, fmt.Errorf("delete credentials: %w", err)
	}
	return true, nil
}

// OAuthConfig returns the OAuth configuration for serverURL built from
// stored credentials, or nil if the server has never been authorized.
// Refreshed tokens are written back to the store.
func (s *Store) OAuthConfig(serverURL string) (*transport.OAuthConfig, error) {
	c, err := s.Load(serverURL)
	if err != nil || c == nil {
		return nil, err
	}
	return &transport.OAuthConfig{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURI:  c.RedirectURI,
		Scopes:       c.Scopes,
		TokenStore:   &tokenStore{store: s, serverURL: serverURL},
		PKCEEnabled:  true,
	}, nil
}

// tokenStore adapts Store to transport.TokenStore for one server.
type tokenStore struct {
	store     *Store
	serverURL string
}

func (t *tokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c, err := t.store.Load(t.serverURL)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Token == nil {
		return nil, transport.ErrNoToken
	}
	return c.Token, nil
}

func (t *tokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.store.mu.Lock()
	defer t.store.mu.Unlock()
	c, err := t.store.load(t.serverURL)
	if err != nil {
		return err
	}
	if c == nil {
		c = &Credentials{ServerURL: t.serverURL}
	}
	c.Token = token
	return t.store.save(c)
}
