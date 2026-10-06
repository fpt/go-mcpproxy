package auth_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/auth"
	"github.com/fpt/go-mcpproxy/internal/authtest"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func allowAll(string) error { return nil }

func newUpstream(t *testing.T, store *auth.Store, cfg remote.Config) *app.Upstream {
	t.Helper()
	u, err := app.New(app.Options{
		Dialer:       app.NewRemoteDialer(cfg, allowAll, store.OAuthConfig),
		StartTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func whoami(t *testing.T, u *app.Upstream) (string, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = "whoami"
	res, err := u.CallTool(context.Background(), req)
	require.NoError(t, err)
	return res.Content[0].(mcp.TextContent).Text, res.IsError
}

func authorize(t *testing.T, store *auth.Store, cfg remote.Config) {
	t.Helper()
	err := auth.Authorize(context.Background(), store, auth.Options{
		Remote:      cfg,
		OpenBrowser: authtest.Browser,
		Timeout:     10 * time.Second,
	})
	require.NoError(t, err)
}

func TestAuthorizeAndCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  func(*authtest.Server) string
	}{
		{"streamable http", (*authtest.Server).MCPURL},
		{"sse", (*authtest.Server).SSEURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := authtest.NewServer(t)
			store := auth.NewStore(t.TempDir())
			cfg := remote.Config{URL: tc.url(srv)}

			// Before authorization: a clear error with the command to run.
			u := newUpstream(t, store, cfg)
			err := u.Start(context.Background())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "mcpproxy auth "+cfg.URL)

			authorize(t, store, cfg)

			creds, err := store.Load(cfg.URL)
			require.NoError(t, err)
			require.NotNil(t, creds)
			assert.Equal(t, "client-1", creds.ClientID)
			assert.True(t, strings.HasPrefix(creds.RedirectURI, "http://127.0.0.1:"))
			require.NotNil(t, creds.Token)

			// The same Upstream picks up the new credentials on its next call.
			out, isErr := whoami(t, u)
			require.False(t, isErr, out)
			assert.Equal(t, "hello "+creds.Token.AccessToken, out)
		})
	}
}

func TestCredentialFilePermissions(t *testing.T) {
	srv := authtest.NewServer(t)
	dir := filepath.Join(t.TempDir(), "credentials")
	store := auth.NewStore(dir)
	authorize(t, store, remote.Config{URL: srv.MCPURL()})

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := entries[0].Info()
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dirInfo, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())
}

func TestExpiredTokenIsRefreshedAndPersisted(t *testing.T) {
	srv := authtest.NewServer(t)
	srv.TokenTTL = 2 * time.Second
	store := auth.NewStore(t.TempDir())
	cfg := remote.Config{URL: srv.MCPURL()}
	authorize(t, store, cfg)
	first, err := store.Load(cfg.URL)
	require.NoError(t, err)

	time.Sleep(2100 * time.Millisecond)

	u := newUpstream(t, store, cfg)
	require.NoError(t, u.Start(context.Background()))
	out, isErr := whoami(t, u)
	require.False(t, isErr, out)
	assert.Equal(t, 1, srv.Refreshes)

	second, err := store.Load(cfg.URL)
	require.NoError(t, err)
	assert.NotEqual(t, first.Token.AccessToken, second.Token.AccessToken)
	assert.Equal(t, "hello "+second.Token.AccessToken, out)
}

func TestRevokedTokenReportsAuthHint(t *testing.T) {
	srv := authtest.NewServer(t)
	store := auth.NewStore(t.TempDir())
	cfg := remote.Config{URL: srv.MCPURL()}
	authorize(t, store, cfg)

	u := newUpstream(t, store, cfg)
	require.NoError(t, u.Start(context.Background()))
	srv.Revoke()

	out, isErr := whoami(t, u)
	require.True(t, isErr)
	assert.Contains(t, out, "mcpproxy auth "+cfg.URL)

	// Re-authorizing reuses the stored client registration.
	authorize(t, store, cfg)
	creds, err := store.Load(cfg.URL)
	require.NoError(t, err)
	assert.Equal(t, "client-1", creds.ClientID)
	out, isErr = whoami(t, u)
	require.False(t, isErr, out)
}

func TestAuthorizeNotRequired(t *testing.T) {
	srv := authtest.NewServer(t)
	srv.RequireAuth = false
	store := auth.NewStore(t.TempDir())
	err := auth.Authorize(context.Background(), store, auth.Options{
		Remote: remote.Config{URL: srv.MCPURL()},
	})
	assert.ErrorIs(t, err, auth.ErrNotRequired)
}

func TestStoreKeyNormalizationAndDelete(t *testing.T) {
	store := auth.NewStore(t.TempDir())
	require.NoError(t, store.Save(&auth.Credentials{
		ServerURL: "https://MCP.example.com/mcp/",
		ClientID:  "c",
	}))
	c, err := store.Load("https://mcp.example.com/mcp")
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, "c", c.ClientID)

	none, err := store.Load("https://mcp.example.com/other")
	require.NoError(t, err)
	assert.Nil(t, none)

	deleted, err := store.Delete("https://mcp.example.com/mcp")
	require.NoError(t, err)
	assert.True(t, deleted)
	deleted, err = store.Delete("https://mcp.example.com/mcp")
	require.NoError(t, err)
	assert.False(t, deleted)
}
