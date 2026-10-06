package allowlist_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fpt/go-mcpproxy/internal/allowlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigAddRemoveSave(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "nested", "config.json")

	cfg, err := allowlist.LoadConfig(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Allow)

	assert.True(t, cfg.Add("/opt/a"))
	assert.False(t, cfg.Add("/opt/a"))
	assert.True(t, cfg.Add("~/bin/b"))
	// "~/" entries compare equal to their expanded form.
	assert.False(t, cfg.Add(filepath.Join(home, "bin", "b")))
	require.NoError(t, cfg.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	loaded, err := allowlist.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"/opt/a", "~/bin/b"}, loaded.Allow)

	assert.True(t, loaded.Remove(filepath.Join(home, "bin", "b")))
	assert.False(t, loaded.Remove("/opt/missing"))
	assert.Equal(t, []string{"/opt/a"}, loaded.Allow)
}

func TestCanonicalEntry(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)

	tests := []struct {
		name    string
		arg     string
		want    string
		wantErr bool
	}{
		{"absolute", "/opt/server", "/opt/server", false},
		{"relative", "output/server", filepath.Join(wd, "output", "server"), false},
		{"home", "~/bin/server", filepath.Join(home, "bin", "server"), false},
		{"pattern", "~/src/*/output/**", filepath.Join(home, "src", "*", "output", "**"), false},
		{"bad pattern", "/opt/[", "", true},
		{"unknown bare name", "no-such-command-mcpproxy", "", true},
		{"empty", " ", "", true},
		{"url", "https://MCP.example.com/mcp/?q=1", "https://mcp.example.com/mcp", false},
		{"url prefix", "https://mcp.example.com/*", "https://mcp.example.com/*", false},
		{"bad url", "https://", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := allowlist.CanonicalEntry(tt.arg)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
