package app_test

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRemoteToolChangeIsPickedUpOnCall(t *testing.T) {
	s := server.NewMCPServer("remote", "1.0")
	s.AddTool(
		mcp.NewTool("old"),
		func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("old"), nil
		},
	)
	httpSrv := httptest.NewServer(server.NewStreamableHTTPServer(s))
	t.Cleanup(httpSrv.Close)

	u, err := app.New(app.Options{
		Dialer: app.NewRemoteDialer(remote.Config{URL: httpSrv.URL},
			func(string) error { return nil }, nil),
		StartTimeout: 10 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = u.Close() })
	require.NoError(t, u.Start(context.Background()))

	// The server changes its tools; the proxy's cache does not know yet.
	s.AddTool(mcp.NewTool("new", mcp.WithString("x", mcp.Required())),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("new:" + req.GetString("x", "")), nil
		})

	res := call(t, u, "new", map[string]any{"x": "1"})
	require.False(t, res.IsError, text(res))
	assert.Equal(t, "new:1", text(res))
	assert.Len(t, u.Tools(), 2)

	// A truly invalid call is still rejected after the re-list.
	res = call(t, u, "new", nil)
	require.True(t, res.IsError)
	assert.Contains(t, text(res), "missing property 'x'")
}
