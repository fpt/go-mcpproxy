package subcmd_test

import (
	"context"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fpt/go-mcpproxy/internal/apptest"
	"github.com/fpt/go-mcpproxy/internal/authtest"
	"github.com/fpt/go-mcpproxy/internal/subcmd"
	"github.com/google/subcommands"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitCallArgs(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		server     string
		tool       string
		rest       []string
		wantErrMsg string
	}{
		{"separate", []string{"godev", "search", "q=1"}, "godev", "search", []string{"q=1"}, ""},
		{"prefixed", []string{"godev__search", "q=1"}, "godev", "search", []string{"q=1"}, ""},
		{
			"tool with underscores",
			[]string{"godev__read_go_doc"},
			"godev",
			"read_go_doc",
			[]string{},
			"",
		},
		{"no server", nil, "", "", nil, "missing server name"},
		{"no tool", []string{"godev"}, "", "", nil, "missing tool name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, tool, rest, err := subcmd.SplitCallArgs(tt.args)
			if tt.wantErrMsg != "" {
				assert.ErrorContains(t, err, tt.wantErrMsg)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.server, server)
			assert.Equal(t, tt.tool, tool)
			assert.Equal(t, tt.rest, rest)
		})
	}
}

func TestParseToolArguments(t *testing.T) {
	got, err := subcmd.ParseToolArguments([]string{
		"name=go mcp", "n:=5", "on:=true", "list:=[1,2]", "eq=a=b", "url=http://x?a:=b",
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"name": "go mcp", "n": 5.0, "on": true, "list": []any{1.0, 2.0}, "eq": "a=b",
		"url": "http://x?a:=b",
	}, got)

	got, err = subcmd.ParseToolArguments([]string{`{"a": [1]}`}, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": []any{1.0}}, got)

	got, err = subcmd.ParseToolArguments([]string{"-"}, strings.NewReader(`{"s": "in"}`))
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"s": "in"}, got)

	got, err = subcmd.ParseToolArguments(nil, nil)
	require.NoError(t, err)
	assert.Empty(t, got)

	for _, bad := range [][]string{{"novalue"}, {"n:=notjson"}, {"=x"}, {"{bad"}} {
		_, err := subcmd.ParseToolArguments(bad, nil)
		assert.Error(t, err, bad)
	}
}

// run executes a subcommand in-process and returns its exit status and
// stdout.
func run(t *testing.T, cmd subcommands.Command, args ...string) (subcommands.ExitStatus, string) {
	t.Helper()
	fs := flag.NewFlagSet(cmd.Name(), flag.ContinueOnError)
	cmd.SetFlags(fs)
	require.NoError(t, fs.Parse(args))

	r, w, err := os.Pipe()
	require.NoError(t, err)
	stdout := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	status := cmd.Execute(context.Background(), fs)
	os.Stdout = stdout
	_ = w.Close()
	return status, <-done
}

func setConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("MCPPROXY_CONFIG", path)
	return path
}

func TestServerCommands(t *testing.T) {
	path := setConfig(t)
	bin := filepath.Join(t.TempDir(), "srv")

	st, out := run(t, &subcmd.LsCmd{})
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Empty(t, out)

	st, out = run(t, &subcmd.AddCmd{}, "-env", "A=1", "local", "--", bin, "serve", "-v")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "added: local\t"+bin+" serve -v\n", out)

	st, _ = run(t, &subcmd.AddCmd{}, "-header", "X-Key: ${K}", "api", "https://api.example.com/mcp")
	require.Equal(t, subcommands.ExitSuccess, st)

	st, out = run(t, &subcmd.AddCmd{}, "api", "https://api.example.com/v2/mcp")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Contains(t, out, "updated: api")

	st, out = run(t, &subcmd.LsCmd{}, "-v")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "# config: "+path+"\n"+
		"api\thttps://api.example.com/v2/mcp\n"+
		"local\t"+bin+" serve -v\t(missing)\n", out)

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), "[servers.local]")
	assert.Contains(t, string(data), `args = ["serve", "-v"]`)

	for _, bad := range [][]string{
		{"bad__name", "--", bin},
		{"x", "https://a/mcp", "extra"},
		{"-env", "A=1", "x", "https://a/mcp"},
		{"-header", "H: v", "x", "--", bin},
		{"x"},
	} {
		st, _ := run(t, &subcmd.AddCmd{}, bad...)
		assert.NotEqual(t, subcommands.ExitSuccess, st, bad)
	}

	st, _ = run(t, &subcmd.RmCmd{}, "api", "nope")
	assert.Equal(t, subcommands.ExitFailure, st, "nothing removed if one name is unknown")
	st, _ = run(t, &subcmd.RmCmd{}, "api")
	require.Equal(t, subcommands.ExitSuccess, st)
	st, out = run(t, &subcmd.LsCmd{})
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "local\t"+bin+" serve -v\n", out)
}

func TestRemoteCommandsEndToEnd(t *testing.T) {
	srv := authtest.NewServer(t)
	setConfig(t)
	defer subcmd.SetOpenBrowser(authtest.Browser)()

	st, _ := run(t, &subcmd.ToolsCmd{}, "remote")
	assert.Equal(t, subcommands.ExitFailure, st, "not configured yet")

	st, _ = run(t, &subcmd.AddCmd{}, "remote", srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)

	st, _ = run(t, &subcmd.ToolsCmd{}, "remote")
	assert.Equal(t, subcommands.ExitFailure, st, "not authorized yet")

	st, _ = run(t, &subcmd.AuthCmd{}, "-timeout", "10s", "remote")
	require.Equal(t, subcommands.ExitSuccess, st)

	st, out := run(t, &subcmd.LsCmd{}, "-v")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Contains(t, out, "(authorized)")

	st, out = run(t, &subcmd.ToolsCmd{}, "remote")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "remote__whoami\t\n", out)

	st, out = run(t, &subcmd.CallCmd{}, "remote", "whoami", "greeting=hi")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.True(t, strings.HasPrefix(out, "hi access-"), out)

	st, _ = run(t, &subcmd.CallCmd{}, "remote__whoami", "greeting:=1")
	assert.Equal(t, subcommands.ExitFailure, st, "schema validation rejects a number")

	st, _ = run(t, &subcmd.LogoutCmd{}, "remote")
	require.Equal(t, subcommands.ExitSuccess, st)
	st, _ = run(t, &subcmd.ToolsCmd{}, "remote")
	assert.Equal(t, subcommands.ExitFailure, st, "credentials deleted")
}

func TestStaticHeaderAuth(t *testing.T) {
	srv := authtest.NewServer(t)
	setConfig(t)
	defer subcmd.SetOpenBrowser(authtest.Browser)()

	// Obtain a token through the OAuth flow, then use it as a static header
	// (via an environment variable) without stored credentials.
	st, _ := run(t, &subcmd.AddCmd{}, "oauth", srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)
	st, _ = run(t, &subcmd.AuthCmd{}, "oauth")
	require.Equal(t, subcommands.ExitSuccess, st)
	st, out := run(t, &subcmd.CallCmd{}, "oauth", "whoami")
	require.Equal(t, subcommands.ExitSuccess, st)
	token := strings.TrimSpace(strings.TrimPrefix(out, "hello "))
	st, _ = run(t, &subcmd.LogoutCmd{}, "oauth")
	require.Equal(t, subcommands.ExitSuccess, st)

	t.Setenv("MCPPROXY_TEST_TOKEN", token)
	st, _ = run(t, &subcmd.AddCmd{}, "-header", "Authorization: Bearer ${MCPPROXY_TEST_TOKEN}",
		"static", srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)
	st, out = run(t, &subcmd.CallCmd{}, "static", "whoami")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "hello "+token+"\n", out)
}

func TestStdioCommands(t *testing.T) {
	dir := t.TempDir()
	setConfig(t)
	for _, name := range []string{"one", "two"} {
		bin := filepath.Join(dir, name)
		apptest.BuildEchoServer(t, bin, "v1")
		st, _ := run(t, &subcmd.AddCmd{}, name, "--", bin)
		require.Equal(t, subcommands.ExitSuccess, st)
	}

	st, out := run(t, &subcmd.ToolsCmd{}, "-q", "echo")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "one__echo\t\ntwo__echo\t\n", out)

	st, out = run(t, &subcmd.CallCmd{}, "two", "echo", "message=hi")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "v1:hi\n", out)

	st, _ = run(t, &subcmd.CallCmd{}, "one__echo")
	assert.Equal(t, subcommands.ExitFailure, st, "missing required argument")

	st, _ = run(t, &subcmd.CallCmd{}, "three", "echo")
	assert.Equal(t, subcommands.ExitFailure, st, "unknown server")
}
