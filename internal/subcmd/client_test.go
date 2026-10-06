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
		name     string
		args     []string
		spec     []string
		rest     []string
		hasError bool
	}{
		{
			"url",
			[]string{"https://x/mcp", "tool", "a=1"},
			[]string{"https://x/mcp"},
			[]string{"tool", "a=1"},
			false,
		},
		{
			"command",
			[]string{"tool", "a=1", "--", "./srv", "serve"},
			[]string{"./srv", "serve"},
			[]string{"tool", "a=1"},
			false,
		},
		{"no target", []string{"tool", "a=1"}, nil, nil, true},
		{"no tool", []string{"https://x/mcp"}, nil, nil, true},
		{"no command", []string{"tool", "--"}, nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, rest, err := subcmd.SplitCallArgs(tt.args)
			if tt.hasError {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.spec, spec)
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

func setConfig(t *testing.T) {
	t.Helper()
	t.Setenv("MCPPROXY_CONFIG", filepath.Join(t.TempDir(), "config.json"))
}

func TestRemoteCommandsEndToEnd(t *testing.T) {
	srv := authtest.NewServer(t)
	setConfig(t)
	defer subcmd.SetOpenBrowser(authtest.Browser)()

	st, _ := run(t, &subcmd.ToolsCmd{}, srv.MCPURL())
	assert.Equal(t, subcommands.ExitFailure, st, "not in allowlist yet")

	st, _ = run(t, &subcmd.AddCmd{}, srv.URL+"/*")
	require.Equal(t, subcommands.ExitSuccess, st)

	st, _ = run(t, &subcmd.ToolsCmd{}, srv.MCPURL())
	assert.Equal(t, subcommands.ExitFailure, st, "not authorized yet")

	st, _ = run(t, &subcmd.AuthCmd{}, "-timeout", "10s", srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)

	st, out := run(t, &subcmd.ToolsCmd{}, srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "whoami\t\n", out)

	st, out = run(t, &subcmd.CallCmd{}, srv.MCPURL(), "whoami", "greeting=hi")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.True(t, strings.HasPrefix(out, "hi access-"), out)

	st, _ = run(t, &subcmd.CallCmd{}, srv.MCPURL(), "whoami", "greeting:=1")
	assert.Equal(t, subcommands.ExitFailure, st, "schema validation rejects a number")

	st, _ = run(t, &subcmd.LogoutCmd{}, srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)
	st, _ = run(t, &subcmd.ToolsCmd{}, srv.MCPURL())
	assert.Equal(t, subcommands.ExitFailure, st, "credentials deleted")
}

func TestStaticHeaderAuth(t *testing.T) {
	srv := authtest.NewServer(t)
	setConfig(t)
	defer subcmd.SetOpenBrowser(authtest.Browser)()
	st, _ := run(t, &subcmd.AddCmd{}, srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)

	// Obtain a token through the OAuth flow, then use it as a static header
	// without stored credentials.
	st, _ = run(t, &subcmd.AuthCmd{}, srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)
	st, out := run(t, &subcmd.CallCmd{}, srv.MCPURL(), "whoami")
	require.Equal(t, subcommands.ExitSuccess, st)
	token := strings.TrimSpace(strings.TrimPrefix(out, "hello "))
	st, _ = run(t, &subcmd.LogoutCmd{}, srv.MCPURL())
	require.Equal(t, subcommands.ExitSuccess, st)

	st, out = run(t, &subcmd.CallCmd{}, "-header", "Authorization: Bearer "+token,
		srv.MCPURL(), "whoami")
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "hello "+token+"\n", out)
}

func TestStdioCommands(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")
	setConfig(t)
	st, _ := run(t, &subcmd.AddCmd{}, bin)
	require.Equal(t, subcommands.ExitSuccess, st)

	st, out := run(t, &subcmd.ToolsCmd{}, "-q", "echo", "--", bin)
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "echo\t\n", out)

	st, out = run(t, &subcmd.CallCmd{}, "echo", "message=hi", "--", bin)
	require.Equal(t, subcommands.ExitSuccess, st)
	assert.Equal(t, "v1:hi\n", out)

	st, _ = run(t, &subcmd.CallCmd{}, "echo", "--", bin)
	assert.Equal(t, subcommands.ExitFailure, st, "missing required argument")
}
