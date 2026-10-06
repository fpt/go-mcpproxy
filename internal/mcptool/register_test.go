package mcptool_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/apptest"
	"github.com/fpt/go-mcpproxy/internal/mcptool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxyEndToEnd(t *testing.T) {
	ctx := context.Background()
	bin := filepath.Join(t.TempDir(), "echoserver")
	apptest.BuildEchoServer(t, bin, "v1")

	up, err := app.New(app.Options{
		Command: bin,
		Settle:  50 * time.Millisecond,
		Resolve: func(c string) (string, error) { return filepath.EvalSymlinks(c) },
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = up.Close() })
	require.NoError(t, up.Start(ctx))

	s := server.NewMCPServer("proxy", "test", server.WithToolCapabilities(true))
	mcptool.Register(s, up, "mcpproxy_restart")

	c, err := client.NewInProcessClient(s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	listChanged := make(chan struct{}, 8)
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationToolsListChanged) {
			listChanged <- struct{}{}
		}
	})
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test"},
	}})
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"echo", "crash", "mcpproxy_restart"}, listNames(t, c))

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go up.Watch(watchCtx, 20*time.Millisecond)

	apptest.BuildEchoServer(t, bin, "v2")
	select {
	case <-listChanged:
	case <-time.After(30 * time.Second):
		t.Fatal("client was not notified of the tool change")
	}

	tools, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)
	for _, tool := range tools.Tools {
		if tool.Name == "echo" {
			assert.Contains(t, tool.InputSchema.Properties, "text")
		}
	}

	req := mcp.CallToolRequest{}
	req.Params.Name = "echo"
	req.Params.Arguments = map[string]any{"text": "ok"}
	res, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Equal(t, "v2:ok", res.Content[0].(mcp.TextContent).Text)

	req = mcp.CallToolRequest{}
	req.Params.Name = "mcpproxy_restart"
	res, err = c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	assert.Contains(t, res.Content[0].(mcp.TextContent).Text, "status: running (generation 3")
}

func listNames(t *testing.T, c *client.Client) []string {
	t.Helper()
	res, err := c.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}
