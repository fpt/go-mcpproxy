package allowlist_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fpt/go-mcpproxy/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func touchExe(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755) //nolint:gosec
	require.NoError(t, err)
	real, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return real
}

func TestResolve(t *testing.T) {
	dir := t.TempDir()
	exact := touchExe(t, filepath.Join(dir, "exact", "server"))
	globbed := touchExe(t, filepath.Join(dir, "glob", "srv-a"))
	deep := touchExe(t, filepath.Join(dir, "tree", "a", "b", "server"))
	other := touchExe(t, filepath.Join(dir, "other", "server"))
	link := filepath.Join(dir, "exact", "link")
	require.NoError(t, os.Symlink(other, link))

	a, err := allowlist.New([]string{
		filepath.Join(dir, "exact", "server"),
		filepath.Join(dir, "glob", "srv-*"),
		filepath.Join(dir, "tree", "**"),
	})
	require.NoError(t, err)

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{"exact", exact, exact},
		{"glob", globbed, globbed},
		{"recursive", deep, deep},
		{"not listed", other, ""},
		{"symlink into allowed dir resolves to target", link, ""},
		{"missing", filepath.Join(dir, "exact", "missing"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.Resolve(tt.command)
			if tt.want == "" {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRecursivePatternWithWildcardDir(t *testing.T) {
	dir := t.TempDir()
	exe := touchExe(t, filepath.Join(dir, "proj1", "bin", "x", "server"))
	a, err := allowlist.New([]string{filepath.Join(dir, "proj*", "bin", "**")})
	require.NoError(t, err)
	got, err := a.Resolve(exe)
	require.NoError(t, err)
	assert.Equal(t, exe, got)
}

func TestNewRejectsRelativePattern(t *testing.T) {
	_, err := allowlist.New([]string{"bin/server"})
	assert.Error(t, err)
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	exe := touchExe(t, filepath.Join(dir, "server"))

	a, err := allowlist.Load(filepath.Join(dir, "missing.json"))
	require.NoError(t, err)
	_, err = a.Resolve(exe)
	assert.ErrorIs(t, err, allowlist.ErrNotAllowed)

	cfg := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(cfg, []byte(`{"allow":["`+exe+`"]}`), 0o600))
	a, err = allowlist.Load(cfg)
	require.NoError(t, err)
	_, err = a.Resolve(exe)
	assert.NoError(t, err)
}
