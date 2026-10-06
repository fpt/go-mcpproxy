package mcptool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const defaultMaxResults = 5

func searchToolDef(opts Options) mcp.Tool {
	desc := fmt.Sprintf(
		"Search the tools of the MCP server %s, which is under development behind mcpproxy. "+
			"Its tools change when it is rebuilt, so the client's own tool search may not see "+
			"them or may have outdated schemas. This searches the build that is running now "+
			"and returns full tool definitions.\n\n"+
			"Query forms:\n"+
			"- \"select:name1,name2\": fetch these exact tools by name\n"+
			"- \"keyword another\": keyword search, up to max_results best matches\n"+
			"- \"+word keyword\": require \"word\" in the tool name, rank by remaining terms\n"+
			"- \"\": list all tools",
		opts.ServerName,
	)
	if opts.CallTool != "" {
		desc += fmt.Sprintf("\n\nCall a found tool directly if you have it, otherwise via %s.",
			opts.CallTool)
	}
	return mcp.NewTool(
		opts.SearchTool,
		mcp.WithDescription(desc),
		mcp.WithString("query", mcp.Required(),
			mcp.Description(`Search query; see the tool description for the forms.`)),
		mcp.WithNumber("max_results", mcp.DefaultNumber(defaultMaxResults),
			mcp.Description("Maximum number of results for keyword searches; 0 for no limit.")),
		mcp.WithReadOnlyHintAnnotation(true),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
		mcp.WithTitleAnnotation("Search proxied MCP server tools"),
	)
}

func searchHandler(u *app.Upstream, opts Options) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.GetString("query", "")
		maxResults := req.GetInt("max_results", defaultMaxResults)

		refreshErr := u.Refresh(ctx)
		tools := u.Tools()
		if refreshErr != nil && len(tools) == 0 {
			return mcp.NewToolResultError(fmt.Sprintf(
				"mcpproxy: upstream is not running: %v", refreshErr,
			)), nil
		}

		res := app.SearchTools(tools, query, maxResults)
		var b strings.Builder
		if refreshErr != nil {
			fmt.Fprintf(&b, "warning: the latest build failed to start; these are the tools "+
				"of the last good build: %v\n\n", refreshErr)
		}
		fmt.Fprintf(&b, "%d of %d matching tools (%d tools total)", len(res.Tools), res.Total,
			len(tools))
		if len(res.Missing) > 0 {
			fmt.Fprintf(&b, "; not found: %s", strings.Join(res.Missing, ", "))
		}
		b.WriteString("\n")
		if len(res.Tools) == 0 {
			fmt.Fprintf(&b, "available tools: %s\n", strings.Join(toolNames(tools), ", "))
		} else {
			b.WriteString("<functions>\n")
			for _, t := range res.Tools {
				line, err := json.Marshal(toolDefinition(t))
				if err != nil {
					return nil, fmt.Errorf("encode tool %q: %w", t.Name, err)
				}
				fmt.Fprintf(&b, "<function>%s</function>\n", line)
			}
			b.WriteString("</functions>\n")
			if opts.CallTool != "" {
				fmt.Fprintf(
					&b,
					"\nIf a tool is not in your tool list (or its schema there differs), "+
						"call it with %s {\"name\": ..., \"arguments\": {...}}.\n",
					opts.CallTool,
				)
			}
		}
		return mcp.NewToolResultText(b.String()), nil
	}
}

// definition is the subset of a tool definition an agent needs to call it.
type definition struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"parameters"`
}

func toolDefinition(t mcp.Tool) definition {
	schema := t.RawInputSchema
	if len(schema) == 0 {
		schema, _ = json.Marshal(t.InputSchema)
	}
	return definition{Name: t.Name, Description: t.Description, InputSchema: schema}
}

func callToolDef(opts Options) mcp.Tool {
	desc := fmt.Sprintf(
		"Call a tool of the MCP server %s, which is under development behind mcpproxy, by name. "+
			"Use this for tools that are missing from your tool list or whose schema there is "+
			"outdated because the server was rebuilt.",
		opts.ServerName,
	)
	if opts.SearchTool != "" {
		desc += fmt.Sprintf(" Find tools and their current schemas with %s.", opts.SearchTool)
	}
	return mcp.NewTool(
		opts.CallTool,
		mcp.WithDescription(desc),
		mcp.WithString("name", mcp.Required(), mcp.Description("Tool name")),
		mcp.WithObject("arguments",
			mcp.Description("Tool arguments, matching the tool's input schema")),
		mcp.WithTitleAnnotation("Call proxied MCP server tool"),
	)
}

func callHandler(u *app.Upstream, opts Options) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if opts.reserved(name) {
			return mcp.NewToolResultError(fmt.Sprintf(
				"mcpproxy: %q is a proxy built-in tool; call it directly", name,
			)), nil
		}
		var args any
		if v, ok := req.GetArguments()["arguments"]; ok && v != nil {
			args = v
		}
		inner := mcp.CallToolRequest{}
		inner.Params.Name = name
		inner.Params.Arguments = args
		inner.Params.Meta = req.Params.Meta
		return u.CallTool(ctx, inner)
	}
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}
