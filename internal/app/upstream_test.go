package app_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/apptest"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newUpstream(t *testing.T, bin string) *app.Upstream {
	t.Helper()
	u, err := app.New(app.Options{
		Command:      bin,
		Settle:       50 * time.Millisecond,
		StartTimeout: 10 * time.Second,
		Resolve:      func(c string) (string, error) { return filepath.EvalSymlinks(c) },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = u.Close() })
	return u
}

func call(t *testing.T, u *app.Upstream, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := u.CallTool(context.Background(), req)
	require.NoError(t, err)
	return res
}

func text(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if tc, ok := res.Content[0].(mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}

func TestUpstreamRestartsOnRebuild(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")

	u := newUpstream(t, bin)
	require.NoError(t, u.Start(context.Background()))

	res := call(t, u, "echo", map[string]any{"message": "hi"})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "v1:hi", text(res))

	apptest.BuildEchoServer(t, bin, "v2")

	// A call shaped for v1 is rejected by the proxy with v2's schema.
	res = call(t, u, "echo", map[string]any{"message": "hi"})
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "do not match the input schema")
	assert.Contains(t, text(res), `"text"`)
	assert.Equal(t, 2, u.Status().Generation)

	res = call(t, u, "echo", map[string]any{"text": "hi", "upper": true})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "v2:HI", text(res))
}

func TestUpstreamValidationRejectsWrongType(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v2")
	u := newUpstream(t, bin)
	require.NoError(t, u.Start(context.Background()))

	res := call(t, u, "echo", map[string]any{"text": "hi", "upper": "yes"})
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "/upper")
}

func TestUpstreamUnknownTool(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")
	u := newUpstream(t, bin)
	require.NoError(t, u.Start(context.Background()))

	res := call(t, u, "nope", nil)
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "Available tools: crash, echo")
}

func TestUpstreamBrokenBuildThenFix(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")
	u := newUpstream(t, bin)
	require.NoError(t, u.Start(context.Background()))

	apptest.BuildEchoServer(t, bin, "broken")
	res := call(t, u, "echo", map[string]any{"message": "hi"})
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "something is broken in this build")
	// Last known tools stay listed so the agent can retry after a fix.
	assert.Len(t, u.Tools(), 2)

	apptest.BuildEchoServer(t, bin, "v1")
	res = call(t, u, "echo", map[string]any{"message": "again"})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "v1:again", text(res))
}

func TestUpstreamRecoversFromCrash(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")
	u := newUpstream(t, bin)
	require.NoError(t, u.Start(context.Background()))

	res := call(t, u, "crash", nil)
	require.True(t, res.IsError)
	assert.False(t, u.Status().Running)

	res = call(t, u, "echo", map[string]any{"message": "back"})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "v1:back", text(res))
}

func TestUpstreamStartBeforeBuild(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	u := newUpstream(t, bin)
	require.Error(t, u.Start(context.Background()))

	apptest.BuildEchoServer(t, bin, "v1")
	res := call(t, u, "echo", map[string]any{"message": "late"})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "v1:late", text(res))
}

func TestUpstreamWatchNotifiesToolChange(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")
	u := newUpstream(t, bin)
	changed := make(chan []mcp.Tool, 4)
	u.OnToolsChanged(func(tools []mcp.Tool) { changed <- tools })
	require.NoError(t, u.Start(context.Background()))
	<-changed // initial tool set

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go u.Watch(ctx, 20*time.Millisecond)

	apptest.BuildEchoServer(t, bin, "v2")
	select {
	case tools := <-changed:
		require.Len(t, tools, 2)
		i := slices.IndexFunc(tools, func(t mcp.Tool) bool { return t.Name == "echo" })
		require.GreaterOrEqual(t, i, 0)
		assert.Contains(t, tools[i].InputSchema.Properties, "text")
	case <-time.After(30 * time.Second):
		t.Fatal("no tool change after rebuild")
	}
}

func TestMain(m *testing.M) {
	if _, err := exec.LookPath("go"); err != nil {
		os.Exit(0)
	}
	os.Exit(m.Run())
}
