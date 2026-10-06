package mcptool_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/fpt/go-mcpproxy/internal/apptest"
	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/hub"
	"github.com/fpt/go-mcpproxy/internal/mcptool"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type proxy struct {
	dir         string
	cfgPath     string
	hub         *hub.Hub
	client      *client.Client
	listChanged chan struct{}
}

// newProxy builds the echoserver fixture twice (servers "a" and "b", variant
// v1), serves both through a hub, and connects an in-process client.
func newProxy(t *testing.T, poll time.Duration) *proxy {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	p := &proxy{
		dir:         dir,
		cfgPath:     filepath.Join(dir, "config.toml"),
		listChanged: make(chan struct{}, 64),
	}
	cfg := &config.Config{Servers: map[string]config.Server{}}
	for _, name := range []string{"a", "b"} {
		bin := p.bin(name)
		apptest.BuildEchoServer(t, bin, "v1")
		cfg.Servers[name] = config.Server{Command: bin}
	}
	require.NoError(t, cfg.Save(p.cfgPath))

	p.hub = hub.New(
		hub.Options{Poll: poll, Settle: 50 * time.Millisecond, StartTimeout: 10 * time.Second},
	)
	t.Cleanup(p.hub.Close)

	s := server.NewMCPServer("proxy", "test", server.WithToolCapabilities(true))
	mcptool.Register(s, p.hub)
	loaded, err := config.Load(p.cfgPath)
	require.NoError(t, err)
	p.hub.Apply(ctx, loaded)

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

func (p *proxy) bin(name string) string { return filepath.Join(p.dir, name) }

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

func (p *proxy) names(t *testing.T) []string {
	t.Helper()
	res, err := p.client.ListTools(context.Background(), mcp.ListToolsRequest{})
	require.NoError(t, err)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// waitFor polls cond until it holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

var builtins = []string{
	mcptool.CallTool, mcptool.RestartTool, mcptool.SearchTool, mcptool.StatusTool,
}

func withBuiltins(names ...string) []string {
	out := append(slices.Clone(names), builtins...)
	slices.Sort(out)
	return out
}

func TestToolsArePrefixedPerServer(t *testing.T) {
	p := newProxy(t, 0)
	assert.Equal(t, withBuiltins("a__crash", "a__echo", "b__crash", "b__echo"), p.names(t))

	out, isErr := p.call(t, "b__echo", map[string]any{"message": "hi"})
	require.False(t, isErr, out)
	assert.Equal(t, "v1:hi", out)

	// Validation messages use the prefixed names the agent sees.
	out, isErr = p.call(t, "a__echo", nil)
	assert.True(t, isErr)
	assert.Contains(t, out, `arguments for "a__echo"`)
}

func TestRebuildOfOneServer(t *testing.T) {
	p := newProxy(t, 20*time.Millisecond)
	apptest.BuildEchoServer(t, p.bin("a"), "v2")

	waitFor(t, func() bool {
		out, isErr := p.call(t, "a__echo", map[string]any{"text": "x"})
		return !isErr && out == "v2:x"
	})
	// The other server is untouched.
	out, isErr := p.call(t, "b__echo", map[string]any{"message": "still v1"})
	require.False(t, isErr, out)
	assert.Equal(t, "v1:still v1", out)
}

func TestConfigHotReload(t *testing.T) {
	p := newProxy(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.hub.WatchConfig(ctx, p.cfgPath, 20*time.Millisecond)

	// Add a server.
	cBin := p.bin("c")
	apptest.BuildEchoServer(t, cBin, "v2")
	cfg, err := config.Load(p.cfgPath)
	require.NoError(t, err)
	cfg.Servers["c"] = config.Server{Command: cBin}
	require.NoError(t, cfg.Save(p.cfgPath))
	waitFor(t, func() bool { return slices.Contains(p.names(t), "c__echo") })
	out, isErr := p.call(t, "c__echo", map[string]any{"text": "new server"})
	require.False(t, isErr, out)
	assert.Equal(t, "v2:new server", out)

	// Remove one.
	delete(cfg.Servers, "a")
	require.NoError(t, cfg.Save(p.cfgPath))
	waitFor(t, func() bool { return !slices.Contains(p.names(t), "a__echo") })
	assert.Equal(t, withBuiltins("b__crash", "b__echo", "c__crash", "c__echo"), p.names(t))

	// Change one: b gets an extra argument, which restarts it.
	cfg.Servers["b"] = config.Server{Command: p.bin("b"), Env: map[string]string{"X": "1"}}
	require.NoError(t, cfg.Save(p.cfgPath))
	waitFor(t, func() bool {
		b, ok := p.hub.Get("b")
		return ok && b.Server.Env["X"] == "1" && b.Upstream.Status().Running
	})

	// An invalid file keeps the current servers and is reported.
	require.NoError(t, os.WriteFile(p.cfgPath, []byte("[servers.b\n"), 0o600))
	waitFor(t, func() bool { return p.hub.ConfigError() != nil })
	out, isErr = p.call(t, "b__echo", map[string]any{"message": "kept"})
	require.False(t, isErr, out)
	out, _ = p.call(t, mcptool.StatusTool, nil)
	assert.Contains(t, out, "config error")

	// Fixing it clears the error.
	require.NoError(t, cfg.Save(p.cfgPath))
	waitFor(t, func() bool { return p.hub.ConfigError() == nil })
}

func TestListChangedOnRebuild(t *testing.T) {
	p := newProxy(t, 20*time.Millisecond)
	for len(p.listChanged) > 0 {
		<-p.listChanged
	}
	apptest.BuildEchoServer(t, p.bin("a"), "v2")
	select {
	case <-p.listChanged:
	case <-time.After(30 * time.Second):
		t.Fatal("client was not notified of the tool change")
	}
}

// TestSearchAndCallAfterUnseenRebuild covers a client whose tool list is
// stale: the rebuild happens without the background watcher, so no
// list_changed is sent, yet search and call reach the new build.
func TestSearchAndCallAfterUnseenRebuild(t *testing.T) {
	p := newProxy(t, 0)
	apptest.BuildEchoServer(t, p.bin("a"), "v2")

	out, isErr := p.call(t, mcptool.SearchTool, map[string]any{"query": "+a__ echo"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "1 of 1 matching tools (4 tools total)")
	assert.Contains(t, out, `<function>{"name":"a__echo","parameters":{`)
	assert.Contains(t, out, `"text"`)

	out, isErr = p.call(t, mcptool.SearchTool, map[string]any{"query": "select:b__echo,a__gone"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "not found: a__gone")
	assert.Contains(t, out, `"name":"b__echo"`)

	out, isErr = p.call(t, mcptool.CallTool, map[string]any{
		"name":      "a__echo",
		"arguments": map[string]any{"text": "via call", "upper": true},
	})
	require.False(t, isErr, out)
	assert.Equal(t, "v2:VIA CALL", out)

	out, isErr = p.call(t, mcptool.CallTool, map[string]any{
		"name":      "a__echo",
		"arguments": map[string]any{"message": "old shape"},
	})
	assert.True(t, isErr)
	assert.Contains(t, out, "missing property 'text'")

	out, isErr = p.call(t, mcptool.CallTool, map[string]any{"name": "zz__echo"})
	assert.True(t, isErr)
	assert.Contains(t, out, `no server named "zz". Servers: a, b`)

	out, isErr = p.call(t, mcptool.CallTool, map[string]any{"name": mcptool.StatusTool})
	assert.True(t, isErr)
	assert.Contains(t, out, "built-in tools are called directly")
}

func TestStatusAndRestart(t *testing.T) {
	p := newProxy(t, 0)

	out, isErr := p.call(t, mcptool.StatusTool, nil)
	require.False(t, isErr, out)
	assert.Contains(t, out, "[a] "+p.bin("a"))
	assert.Contains(t, out, "tools: a__crash, a__echo")

	out, isErr = p.call(t, mcptool.RestartTool, map[string]any{"server": "b"})
	require.False(t, isErr, out)
	assert.Contains(t, out, "[b]")
	assert.Contains(t, out, "status: running (generation 2")

	out, isErr = p.call(t, mcptool.RestartTool, map[string]any{"server": "nope"})
	assert.True(t, isErr)
	assert.Contains(t, out, "no server named")
}

func TestServerNotBuiltYetAndBroken(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	h := hub.New(hub.Options{Settle: 20 * time.Millisecond, StartTimeout: 10 * time.Second})
	t.Cleanup(h.Close)
	s := server.NewMCPServer("proxy", "test", server.WithToolCapabilities(true))
	mcptool.Register(s, h)

	later := filepath.Join(dir, "later")
	h.Apply(ctx, &config.Config{Servers: map[string]config.Server{"later": {Command: later}}})
	b, ok := h.Get("later")
	require.True(t, ok)
	require.NotNil(t, b.Upstream)
	assert.False(t, b.Upstream.Status().Running)

	apptest.BuildEchoServer(t, later, "v1")
	req := mcp.CallToolRequest{}
	req.Params.Name = "echo"
	req.Params.Arguments = map[string]any{"message": "built"}
	res, err := b.Upstream.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
}
