// Package mcptool exposes the tools of a hub's backends on an MCP server,
// together with mcpproxy's built-in tools.
package mcptool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/hub"
	"github.com/fpt/go-mcpproxy/internal/wrapper"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Built-in tool names. They contain no config.ToolSeparator, so they can
// never collide with a backend tool.
const (
	StatusTool  = "mcpproxy_status"
	RestartTool = "mcpproxy_restart"
	SearchTool  = "mcpproxy_search_tools"
	CallTool    = "mcpproxy_call_tool"
)

// Register publishes every backend's tools on s as "<server>__<tool>" and
// republishes them whenever the hub reports a change.
//
// s must be created with server.WithToolCapabilities(true) so that clients
// are sent notifications/tools/list_changed.
func Register(s *server.MCPServer, h *hub.Hub) {
	var (
		mu   sync.Mutex
		last []byte
	)
	sync := func() {
		mu.Lock()
		defer mu.Unlock()
		defer func() {
			// SetTools panics on tool definitions it considers invalid; a
			// broken backend build must not take the proxy down with it.
			if r := recover(); r != nil {
				slog.Error("failed to publish backend tools", "panic", r)
			}
		}()
		tools := serverTools(h)
		// Several events (a backend's start and the config reload that
		// caused it) can report the same change; notify clients only once.
		defs := make([]mcp.Tool, 0, len(tools))
		for _, t := range tools {
			defs = append(defs, t.Tool)
		}
		sig, err := json.Marshal(defs)
		if err == nil && bytes.Equal(sig, last) {
			return
		}
		last = sig
		s.SetTools(tools...)
	}
	h.OnChange(sync)
	sync()
}

func serverTools(h *hub.Hub) []server.ServerTool {
	backends := h.Backends()
	var out []server.ServerTool
	for _, b := range backends {
		if b.Upstream == nil {
			continue
		}
		for _, t := range b.Upstream.Tools() {
			out = append(out, server.ServerTool{
				Tool:    prefixed(b.Name, t),
				Handler: forward(b, t.Name),
			})
		}
	}
	wrappers := h.Wrappers()
	for _, name := range slices.Sorted(maps.Keys(wrappers)) {
		out = append(out, server.ServerTool{
			Tool:    wrapper.Tool(name, wrappers[name]),
			Handler: wrapper.Handler(wrappers[name]),
		})
	}
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name)
	}
	return append(out,
		server.ServerTool{Tool: statusToolDef(), Handler: statusHandler(h)},
		server.ServerTool{Tool: restartToolDef(), Handler: restartHandler(h)},
		server.ServerTool{Tool: searchToolDef(names), Handler: searchHandler(h)},
		server.ServerTool{Tool: callToolDef(names), Handler: callHandler(h)},
	)
}

func prefixed(serverName string, t mcp.Tool) mcp.Tool {
	t.Name = serverName + config.ToolSeparator + t.Name
	return t
}

func forward(b hub.Backend, toolName string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		req.Params.Name = toolName
		return b.Upstream.CallTool(ctx, req)
	}
}

// splitToolName splits "<server>__<tool>".
func splitToolName(name string) (string, string, bool) {
	return strings.Cut(name, config.ToolSeparator)
}

func statusToolDef() mcp.Tool {
	return mcp.NewTool(StatusTool,
		mcp.WithDescription("Show the MCP servers behind mcpproxy: whether each is running, "+
			"its tools, and the last error (e.g. a failed build's stderr or an authorization "+
			"problem). Use it when a proxied tool fails or is missing."),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithTitleAnnotation("mcpproxy status"),
	)
}

func statusHandler(h *hub.Hub) server.ToolHandlerFunc {
	return func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var b strings.Builder
		if err := h.ConfigError(); err != nil {
			fmt.Fprintf(
				&b,
				"config error (the servers below are from the last valid config): %v\n\n",
				err,
			)
		}
		backends := h.Backends()
		wrappers := h.Wrappers()
		if len(backends) == 0 && len(wrappers) == 0 {
			b.WriteString("No servers are configured. The user can add one with " +
				"`mcpproxy add <name> -- <command> [args...]` or `mcpproxy add <name> <url>`.\n")
		}
		for _, be := range backends {
			b.WriteString(formatStatus(be))
			b.WriteString("\n")
		}
		for _, name := range slices.Sorted(maps.Keys(wrappers)) {
			w := wrappers[name]
			dir := w.Dir
			if dir == "" {
				dir = "(mcpproxy's working directory)"
			}
			fmt.Fprintf(&b, "[%s] wrapper tool: %s\ndir: %s\n\n", name, w.Command, dir)
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

func restartToolDef() mcp.Tool {
	return mcp.NewTool(
		RestartTool,
		mcp.WithDescription("Restart one MCP server behind mcpproxy (or reconnect to a remote "+
			"one) and report its status. Rebuilds are normally picked up automatically; use this "+
			"when the server depends on files mcpproxy does not watch."),
		mcp.WithString(
			"server",
			mcp.Required(),
			mcp.Description("Server name, as in mcpproxy_status"),
		),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithTitleAnnotation("Restart a proxied MCP server"),
	)
}

func restartHandler(h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name := req.GetString("server", "")
		b, ok := h.Get(name)
		if !ok {
			return mcp.NewToolResultError(unknownServer(h, name)), nil
		}
		if b.Upstream == nil {
			return mcp.NewToolResultError(formatStatus(b)), nil
		}
		err := b.Upstream.Restart(ctx)
		text := formatStatus(b)
		if err != nil {
			return mcp.NewToolResultError(text), nil
		}
		return mcp.NewToolResultText(text), nil
	}
}

func formatStatus(b hub.Backend) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%s] %s\n", b.Name, b.Server.String())
	if b.Upstream == nil {
		fmt.Fprintf(&sb, "status: not usable: %v\n", b.Err)
		return sb.String()
	}
	st := b.Upstream.Status()
	if st.Running {
		fmt.Fprintf(&sb, "status: running (generation %d, started %s)\n",
			st.Generation, st.StartedAt.Format(time.RFC3339))
	} else {
		sb.WriteString("status: not running\n")
	}
	tools := make([]string, 0, len(st.Tools))
	for _, t := range st.Tools {
		tools = append(tools, b.Name+config.ToolSeparator+t)
	}
	fmt.Fprintf(&sb, "tools: %s\n", strings.Join(tools, ", "))
	if st.LastError != "" {
		fmt.Fprintf(&sb, "error: %s\n", st.LastError)
	} else if !st.Running && st.Stderr != "" {
		fmt.Fprintf(&sb, "stderr:\n%s\n", st.Stderr)
	}
	return sb.String()
}

func unknownServer(h *hub.Hub, name string) string {
	backends := h.Backends()
	names := make([]string, 0, len(backends))
	for _, b := range backends {
		names = append(names, b.Name)
	}
	return fmt.Sprintf("mcpproxy: no server named %q. Servers: %s", name, strings.Join(names, ", "))
}
