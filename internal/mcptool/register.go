// Package mcptool exposes an app.Upstream's tools on an MCP server.
package mcptool

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Options names the built-in proxy tools. An empty name disables that tool.
type Options struct {
	// RestartTool forces a restart and reports the upstream status.
	RestartTool string
	// SearchTool searches the running build's tools, like Claude Code's
	// ToolSearch, which cannot see tools that changed after a rebuild.
	SearchTool string
	// CallTool calls an upstream tool by name, for tools the client has not
	// (re)loaded definitions for.
	CallTool string
	// ServerName identifies the upstream in tool descriptions.
	ServerName string
}

func (o Options) reserved(name string) bool {
	return name != "" && (name == o.RestartTool || name == o.SearchTool || name == o.CallTool)
}

// Register mirrors the upstream's tools onto s and keeps them in sync on
// every restart, alongside the built-in tools named in opts.
//
// s must be created with server.WithToolCapabilities(true) so that clients
// are sent notifications/tools/list_changed after a rebuild.
func Register(s *server.MCPServer, u *app.Upstream, opts Options) {
	sync := func(tools []mcp.Tool) {
		defer func() {
			// SetTools panics on tool definitions it considers invalid; a
			// broken upstream build must not take the proxy down with it.
			if r := recover(); r != nil {
				slog.Error("failed to publish upstream tools", "panic", r)
			}
		}()
		s.SetTools(serverTools(u, tools, opts)...)
	}
	u.OnToolsChanged(sync)
	sync(u.Tools())
}

func serverTools(u *app.Upstream, tools []mcp.Tool, opts Options) []server.ServerTool {
	out := make([]server.ServerTool, 0, len(tools)+3)
	for _, t := range tools {
		if opts.reserved(t.Name) {
			slog.Warn("upstream tool shadowed by a proxy built-in tool", "tool", t.Name)
			continue
		}
		out = append(out, server.ServerTool{Tool: t, Handler: u.CallTool})
	}
	if opts.RestartTool != "" {
		out = append(out, server.ServerTool{
			Tool:    restartToolDef(opts.RestartTool),
			Handler: restartHandler(u),
		})
	}
	if opts.SearchTool != "" {
		out = append(out, server.ServerTool{
			Tool:    searchToolDef(opts),
			Handler: searchHandler(u, opts),
		})
	}
	if opts.CallTool != "" {
		out = append(out, server.ServerTool{
			Tool:    callToolDef(opts),
			Handler: callHandler(u, opts),
		})
	}
	return out
}

func restartToolDef(name string) mcp.Tool {
	return mcp.NewTool(
		name,
		mcp.WithDescription(
			"Restart the MCP server proxied by mcpproxy and report its status and tools. "+
				"Rebuilds are normally picked up automatically; use this when the server "+
				"depends on files mcpproxy does not watch, or to see why it failed to start.",
		),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithTitleAnnotation("Restart proxied MCP server"),
	)
}

func restartHandler(u *app.Upstream) server.ToolHandlerFunc {
	return func(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		err := u.Restart(ctx)
		text := formatStatus(u.Status())
		if err != nil {
			return mcp.NewToolResultError(text), nil
		}
		return mcp.NewToolResultText(text), nil
	}
}

func formatStatus(st app.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "command: %s %s\n", st.Command, strings.Join(st.Args, " "))
	if st.Running {
		fmt.Fprintf(&b, "status: running (generation %d, started %s)\n",
			st.Generation, st.StartedAt.Format(time.RFC3339))
	} else {
		b.WriteString("status: not running\n")
	}
	fmt.Fprintf(&b, "tools: %s\n", strings.Join(st.Tools, ", "))
	if st.LastError != "" {
		fmt.Fprintf(&b, "error: %s\n", st.LastError)
	} else if !st.Running && st.Stderr != "" {
		fmt.Fprintf(&b, "stderr:\n%s\n", st.Stderr)
	}
	return b.String()
}
