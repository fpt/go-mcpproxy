package mcptool

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/hub"
	"github.com/fpt/go-mcpproxy/internal/wrapper"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

const defaultMaxResults = 5

func serverList(names []string) string {
	if len(names) == 0 {
		return "(none configured yet)"
	}
	return strings.Join(names, ", ")
}

func searchToolDef(servers []string) mcp.Tool {
	desc := fmt.Sprintf(
		"Search the tools of the MCP servers behind mcpproxy (servers: %s). Their tools "+
			"change when a server is rebuilt or the proxy's config changes, so the client's "+
			"own tool search may not see them or may have outdated schemas. This searches "+
			"what is running now and returns full tool definitions. Tool names are "+
			"\"<server>__<tool>\".\n\n"+
			"Query forms:\n"+
			"- \"select:name1,name2\": fetch these exact tools by name\n"+
			"- \"keyword another\": keyword search, up to max_results best matches\n"+
			"- \"+word keyword\": require \"word\" in the tool name, rank by remaining terms\n"+
			"  (e.g. \"+godev__ search\" searches only the server godev)\n"+
			"- \"\": list all tools\n\n"+
			"Call a found tool directly if you have it, otherwise via %s.",
		serverList(servers), CallTool,
	)
	return mcp.NewTool(SearchTool,
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

func searchHandler(h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		query := req.GetString("query", "")
		maxResults := req.GetInt("max_results", defaultMaxResults)

		var tools []mcp.Tool
		var warnings []string
		for _, b := range h.Backends() {
			if b.Upstream == nil {
				warnings = append(warnings, fmt.Sprintf("%s: not usable: %v", b.Name, b.Err))
				continue
			}
			if err := b.Upstream.Refresh(ctx); err != nil {
				warnings = append(warnings, fmt.Sprintf("%s: not running (showing its last known "+
					"tools, if any): %v", b.Name, firstLine(err.Error())))
			}
			for _, t := range b.Upstream.Tools() {
				tools = append(tools, prefixed(b.Name, t))
			}
		}

		wrappers := h.Wrappers()
		for _, name := range slices.Sorted(maps.Keys(wrappers)) {
			tools = append(tools, wrapper.Tool(name, wrappers[name]))
		}

		res := app.SearchTools(tools, query, maxResults)
		var sb strings.Builder
		for _, w := range warnings {
			fmt.Fprintf(&sb, "warning: %s\n", w)
		}
		if len(warnings) > 0 {
			fmt.Fprintf(&sb, "(see %s for details)\n\n", StatusTool)
		}
		fmt.Fprintf(&sb, "%d of %d matching tools (%d tools total)", len(res.Tools), res.Total,
			len(tools))
		if len(res.Missing) > 0 {
			fmt.Fprintf(&sb, "; not found: %s", strings.Join(res.Missing, ", "))
		}
		sb.WriteString("\n")
		if len(res.Tools) == 0 {
			fmt.Fprintf(&sb, "available tools: %s\n", strings.Join(toolNames(tools), ", "))
			return mcp.NewToolResultText(sb.String()), nil
		}
		sb.WriteString("<functions>\n")
		for _, t := range res.Tools {
			line, err := json.Marshal(toolDefinition(t))
			if err != nil {
				return nil, fmt.Errorf("encode tool %q: %w", t.Name, err)
			}
			fmt.Fprintf(&sb, "<function>%s</function>\n", line)
		}
		sb.WriteString("</functions>\n")
		fmt.Fprintf(&sb, "\nIf a tool is not in your tool list (or its schema there differs), "+
			"call it with %s {\"name\": ..., \"arguments\": {...}}.\n", CallTool)
		return mcp.NewToolResultText(sb.String()), nil
	}
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
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

func callToolDef(servers []string) mcp.Tool {
	desc := fmt.Sprintf(
		"Call a tool of an MCP server behind mcpproxy (servers: %s) by its full name "+
			"\"<server>__<tool>\". Use this for tools that are missing from your tool list or "+
			"whose schema there is outdated because the server was rebuilt or reconfigured. "+
			"Find tools and their current schemas with %s.",
		serverList(servers), SearchTool,
	)
	return mcp.NewTool(CallTool,
		mcp.WithDescription(desc),
		mcp.WithString("name", mcp.Required(), mcp.Description(`Tool name, "<server>__<tool>"`)),
		mcp.WithObject("arguments",
			mcp.Description("Tool arguments, matching the tool's input schema")),
		mcp.WithTitleAnnotation("Call proxied MCP server tool"),
	)
}

func callHandler(h *hub.Hub) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		name, err := req.RequireString("name")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		serverName, toolName, ok := splitToolName(name)
		if !ok {
			if w, found := h.Wrappers()[name]; found {
				return wrapper.ToolResult(wrapper.Run(ctx, w)), nil
			}
			return mcp.NewToolResultError(fmt.Sprintf(
				"mcpproxy: %q is not a proxied tool name; use \"<server>__<tool>\" or a "+
					"wrapper tool's name (built-in tools are called directly)", name,
			)), nil
		}
		b, found := h.Get(serverName)
		if !found {
			return mcp.NewToolResultError(unknownServer(h, serverName)), nil
		}
		if b.Upstream == nil {
			return mcp.NewToolResultError(formatStatus(b)), nil
		}
		var args any
		if v, ok := req.GetArguments()["arguments"]; ok && v != nil {
			args = v
		}
		inner := mcp.CallToolRequest{}
		inner.Params.Name = toolName
		inner.Params.Arguments = args
		inner.Params.Meta = req.Params.Meta
		return b.Upstream.CallTool(ctx, inner)
	}
}

func toolNames(tools []mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}
