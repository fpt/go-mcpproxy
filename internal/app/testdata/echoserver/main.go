// Command echoserver is a test fixture whose tool set depends on the
// build-time variant (-ldflags "-X main.variant=v2"), simulating a rebuild
// of an MCP server under development.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

var variant = "v1"

func main() {
	s := server.NewMCPServer("echoserver", variant)

	switch variant {
	case "v1":
		s.AddTool(mcp.NewTool("echo",
			mcp.WithString("message", mcp.Required()),
		), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("v1:" + req.GetString("message", "")), nil
		})
	case "v2":
		s.AddTool(mcp.NewTool("echo",
			mcp.WithString("text", mcp.Required()),
			mcp.WithBoolean("upper"),
		), func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			text := req.GetString("text", "")
			if req.GetBool("upper", false) {
				text = strings.ToUpper(text)
			}
			return mcp.NewToolResultText("v2:" + text), nil
		})
	case "broken":
		fmt.Fprintln(os.Stderr, "panic: something is broken in this build")
		os.Exit(2)
	}

	s.AddTool(mcp.NewTool("crash"), func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		fmt.Fprintln(os.Stderr, "crashing on purpose")
		os.Exit(3)
		return nil, nil
	})

	if err := server.ServeStdio(s); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
