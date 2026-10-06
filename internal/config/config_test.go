package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadSaveRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.toml")

	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Empty(t, cfg.Servers)

	cfg.Servers["godev"] = config.Server{
		Command: "/opt/godev/godevmcp",
		Args:    []string{"serve"},
		Env:     map[string]string{"DEBUG": "1"},
	}
	cfg.Servers["example"] = config.Server{
		URL:     "https://mcp.example.com/mcp",
		Headers: map[string]string{"Authorization": "Bearer ${TOKEN}"},
	}
	require.NoError(t, cfg.Save(path))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	loaded, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, []string{"example", "godev"}, loaded.Names())
	assert.True(t, loaded.Servers["godev"].Equal(cfg.Servers["godev"]))
	assert.True(t, loaded.Servers["example"].Equal(cfg.Servers["example"]))
}

func TestLoadHandWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
# my servers
[servers.godev]
command = "/opt/godev/godevmcp"
args = ["serve"]
watch = ["/opt/godev/config.yaml"]

[servers.remote]
url = "https://mcp.example.com/sse"
transport = "sse"
headers = { X-Api-Key = "${KEY}" }
`), 0o600))
	cfg, err := config.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "https://mcp.example.com/sse", cfg.Servers["remote"].URL)
	assert.Equal(t, "${KEY}", cfg.Servers["remote"].Headers["X-Api-Key"])
	assert.Equal(t, []string{"/opt/godev/config.yaml"}, cfg.Servers["godev"].Watch)
}

func TestLoadRejects(t *testing.T) {
	tests := map[string]string{
		"unknown key":    "[servers.a]\ncommand = \"/x\"\ncomand = \"typo\"\n",
		"both":           "[servers.a]\ncommand = \"/x\"\nurl = \"https://x/mcp\"\n",
		"neither":        "[servers.a]\nargs = [\"x\"]\n",
		"relative":       "[servers.a]\ncommand = \"bin/x\"\n",
		"bad name":       "[servers.\"a__b\"]\ncommand = \"/x\"\n",
		"bad transport":  "[servers.a]\nurl = \"https://x/mcp\"\ntransport = \"ws\"\n",
		"args on url":    "[servers.a]\nurl = \"https://x/mcp\"\nargs = [\"x\"]\n",
		"header on cmd":  "[servers.a]\ncommand = \"/x\"\nheaders = { A = \"b\" }\n",
		"invalid toml":   "[servers.a\n",
		"relative watch": "[servers.a]\ncommand = \"/x\"\nwatch = [\"rel\"]\n",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
			_, err := config.Load(path)
			assert.Error(t, err)
		})
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"godev", "my-server", "s_1", "A1"} {
		assert.NoError(t, config.ValidateName(ok), ok)
	}
	for _, bad := range []string{
		"", "-x", "_x", "a__b", "a b", "a.b",
		"abcdefghijklmnopqrstuvwxyz0123456",
	} {
		assert.Error(t, config.ValidateName(bad), bad)
	}
}

func TestResolveCommand(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	wd, err := os.Getwd()
	require.NoError(t, err)

	got, err := config.ResolveCommand("output/server")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "output", "server"), got)

	got, err = config.ResolveCommand("~/bin/server")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "bin", "server"), got)

	_, err = config.ResolveCommand("no-such-command-mcpproxy")
	assert.Error(t, err)
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("MCPPROXY_TEST_TOKEN", "secret")
	assert.Equal(t, "Bearer secret", config.ExpandEnv("Bearer ${MCPPROXY_TEST_TOKEN}"))
	assert.Equal(t, "price $5 $HOME", config.ExpandEnv("price $5 $HOME"))
	assert.Equal(t, "x", config.ExpandEnv("x${MCPPROXY_TEST_UNSET}"))
}
