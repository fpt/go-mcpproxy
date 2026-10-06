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

var testOptions = mcptool.Options{
	RestartTool: "mcpproxy_restart",
	SearchTool:  "mcpproxy_search_tools",
	CallTool:    "mcpproxy_call_tool",
	ServerName:  "echoserver",
}

type proxy struct {
	bin         string
	up          *app.Upstream
	client      *client.Client
	listChanged chan struct{}
}

// newProxy starts the echoserver fixture (variant v1) behind a proxy server
// and connects an in-process client to it.
func newProxy(t *testing.T) *proxy {
	t.Helper()
	ctx := context.Background()
	p := &proxy{
		bin:         filepath.Join(t.TempDir(), "echoserver"),
		listChanged: make(chan struct{}, 8),
	}
	apptest.BuildEchoServer(t, p.bin, "v1")

	up, err := app.New(app.Options{
		Dialer: apptest.StdioDialer(t, p.bin),
		Settle: 50 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = up.Close() })
	require.NoError(t, up.Start(ctx))
	p.up = up

	s := server.NewMCPServer("proxy", "test", server.WithToolCapabilities(true))
	mcptool.Register(s, up, testOptions)

	c, err := client.NewInProcessClient(s)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	c.OnNotification(func(n mcp.JSONRPCNotification) {
		if n.Method == string(mcp.MethodNotificationToolsListChanged) {
			p.listChanged <- struct{}{}
		}
	})
	require.NoError(t, c.Start(ctx))
	_, err = c.Initialize(ctx, mcp.InitializeRequest{Params: mcp.InitializeParams{
		ProtocolVersion: mcp.LATEST_LEGACY_PROTOCOL_VERSION,
		ClientInfo:      mcp.Implementation{Name: "test"},
	}})
	require.NoError(t, err)
	p.client = c
	return p
}

func (p *proxy) call(t *testing.T, name string, args map[string]any) (string, bool) {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	res, err := p.client.CallTool(context.Background(), req)
	require.NoError(t, err)
	require.NotEmpty(t, res.Content)
	return res.Content[0].(mcp.TextContent).Text, res.IsError
}

func TestProxyEndToEnd(t *testing.T) {
	ctx := context.Background()
	p := newProxy(t)
	c, up, bin, listChanged := p.client, p.up, p.bin, p.listChanged

	assert.ElementsMatch(t, []string{
		"echo", "crash", "mcpproxy_restart", "mcpproxy_search_tools", "mcpproxy_call_tool",
	}, listNames(t, c))

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

// TestSearchAndCallAfterUnseenRebuild covers a client whose tool list is
// stale: the rebuild happens without the background watcher, so no
// list_changed is sent, yet search and call reach the new build.
func TestSearchAndCallAfterUnseenRebuild(t *testing.T) {
	p := newProxy(t)
	apptest.BuildEchoServer(t, p.bin, "v2")

	out, isErr := p.call(t, "mcpproxy_search_tools", map[string]any{"query": "echo"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "1 of 1 matching tools (2 tools total)")
	assert.Contains(t, out, `<function>{"name":"echo","parameters":{`)
	assert.Contains(t, out, `"text"`)
	assert.NotContains(t, out, `"message"`)

	out, isErr = p.call(t, "mcpproxy_search_tools", map[string]any{"query": "select:echo,gone"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "not found: gone")

	out, isErr = p.call(t, "mcpproxy_search_tools", map[string]any{"query": "nothing-matches"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "available tools: crash, echo")

	out, isErr = p.call(t, "mcpproxy_call_tool", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"text": "via call", "upper": true},
	})
	require.False(t, isErr, out)
	assert.Equal(t, "v2:VIA CALL", out)

	// Arguments are still validated against the running build.
	out, isErr = p.call(t, "mcpproxy_call_tool", map[string]any{
		"name":      "echo",
		"arguments": map[string]any{"message": "old shape"},
	})
	assert.True(t, isErr)
	assert.Contains(t, out, "missing property 'text'")

	out, isErr = p.call(t, "mcpproxy_call_tool", map[string]any{"name": "mcpproxy_restart"})
	assert.True(t, isErr)
	assert.Contains(t, out, "proxy built-in tool")
}
