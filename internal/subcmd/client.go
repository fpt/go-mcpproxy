package subcmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/google/subcommands"
	"github.com/mark3labs/mcp-go/mcp"
)

// connect starts an Upstream for one-shot command-line use.
func connect(ctx context.Context, d app.Dialer, verbose bool) (*app.Upstream, error) {
	stderr := io.Discard
	if verbose {
		stderr = os.Stderr
	}
	up, err := app.New(app.Options{
		Dialer:        d,
		StartTimeout:  30 * time.Second,
		Stderr:        stderr,
		ClientVersion: Version,
	})
	if err != nil {
		return nil, err
	}
	if err := up.Start(ctx); err != nil {
		_ = up.Close()
		return nil, err
	}
	return up, nil
}

// ToolsCmd lists the tools of an MCP server.
type ToolsCmd struct {
	target  targetFlags
	query   string
	json    bool
	verbose bool
}

func (*ToolsCmd) Name() string     { return "tools" }
func (*ToolsCmd) Synopsis() string { return "List the tools of an MCP server." }
func (*ToolsCmd) Usage() string {
	return `tools [flags] <url>
tools [flags] -- <command> [args...]:
  List the tools of a remote server or a stdio server command.

`
}

func (p *ToolsCmd) SetFlags(f *flag.FlagSet) {
	p.target.setFlags(f)
	f.StringVar(&p.query, "q", "",
		`Search query, as for mcpproxy_search_tools ("select:a,b", keywords, "+word")`)
	f.BoolVar(&p.json, "json", false, "Print full tool definitions as JSON")
	f.BoolVar(&p.verbose, "v", false, "Show the stderr of a stdio server")
}

func (p *ToolsCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	env, err := loadEnvironment()
	if err != nil {
		return fail(err)
	}
	dialer, err := p.target.dialer(env, f.Args())
	if err != nil {
		return fail(err)
	}
	up, err := connect(ctx, dialer, p.verbose)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = up.Close() }()

	tools := up.Tools()
	if p.query != "" {
		tools = app.SearchTools(tools, p.query, 0).Tools
	}
	if p.json {
		return printJSON(tools)
	}
	for _, t := range tools {
		desc, _, _ := strings.Cut(strings.TrimSpace(t.Description), "\n")
		fmt.Printf("%s\t%s\n", t.Name, desc)
	}
	return subcommands.ExitSuccess
}

// CallCmd calls a tool of an MCP server.
type CallCmd struct {
	target  targetFlags
	json    bool
	verbose bool
	timeout time.Duration
}

func (*CallCmd) Name() string     { return "call" }
func (*CallCmd) Synopsis() string { return "Call a tool of an MCP server." }
func (*CallCmd) Usage() string {
	return `call [flags] <url> <tool> [arguments...]
call [flags] <tool> [arguments...] -- <command> [args...]:
  Call a tool of a remote server or a stdio server command and print the result.
  Arguments are checked against the tool's input schema before calling.
  The exit status is 1 if the tool reports an error.

  Arguments are either a single JSON object, "-" to read a JSON object from
  stdin, or any number of:
    key=value     string value
    key:=json     JSON value (number, boolean, array, object, null)

  Examples:
    mcpproxy call https://mcp.example.com/mcp search query='go mcp' limit:=5
    mcpproxy call echo '{"message": "hi"}' -- ./output/my-server serve

`
}

func (p *CallCmd) SetFlags(f *flag.FlagSet) {
	p.target.setFlags(f)
	f.BoolVar(&p.json, "json", false, "Print the raw result as JSON")
	f.BoolVar(&p.verbose, "v", false, "Show the stderr of a stdio server")
	f.DurationVar(&p.timeout, "timeout", 0, "Timeout for the call (0: none)")
}

func (p *CallCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	spec, rest, err := splitCallArgs(f.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpproxy: %v\n\n", err)
		fmt.Fprint(os.Stderr, p.Usage())
		return subcommands.ExitUsageError
	}
	args, err := parseToolArguments(rest[1:], os.Stdin)
	if err != nil {
		return fail(err)
	}

	env, err := loadEnvironment()
	if err != nil {
		return fail(err)
	}
	dialer, err := p.target.dialer(env, spec)
	if err != nil {
		return fail(err)
	}
	up, err := connect(ctx, dialer, p.verbose)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = up.Close() }()

	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = rest[0]
	req.Params.Arguments = args
	res, err := up.CallTool(ctx, req)
	if err != nil {
		return fail(err)
	}

	if p.json {
		if st := printJSON(res); st != subcommands.ExitSuccess {
			return st
		}
	} else {
		out := os.Stdout
		if res.IsError {
			out = os.Stderr
		}
		printResult(out, res)
	}
	if res.IsError {
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// splitCallArgs separates the target from the tool name and arguments:
// "<url> <tool> args..." or "<tool> args... -- <command> args...".
func splitCallArgs(args []string) (spec, rest []string, err error) {
	if i := slices.Index(args, "--"); i >= 0 {
		spec, rest = args[i+1:], args[:i]
	} else if len(args) > 0 && remote.IsURL(args[0]) {
		spec, rest = args[:1], args[1:]
	} else {
		return nil, nil, errors.New("give a server URL first, or the command after --")
	}
	if len(spec) == 0 {
		return nil, nil, errors.New("missing command after --")
	}
	if len(rest) == 0 {
		return nil, nil, errors.New("missing tool name")
	}
	return spec, rest, nil
}

// parseToolArguments builds the arguments object from command-line words.
func parseToolArguments(words []string, stdin io.Reader) (map[string]any, error) {
	if len(words) == 1 && (words[0] == "-" || strings.HasPrefix(strings.TrimSpace(words[0]), "{")) {
		var data []byte
		if words[0] == "-" {
			var err error
			if data, err = io.ReadAll(stdin); err != nil {
				return nil, fmt.Errorf("read arguments from stdin: %w", err)
			}
		} else {
			data = []byte(words[0])
		}
		var obj map[string]any
		if err := json.Unmarshal(data, &obj); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
		return obj, nil
	}

	args := make(map[string]any, len(words))
	for _, w := range words {
		if key, raw, ok := strings.Cut(w, ":="); ok && key != "" && !strings.Contains(key, "=") {
			var v any
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				return nil, fmt.Errorf("argument %q: invalid JSON after \":=\": %w", w, err)
			}
			args[key] = v
			continue
		}
		key, value, ok := strings.Cut(w, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("argument %q must be key=value or key:=json", w)
		}
		args[key] = value
	}
	return args, nil
}

func printResult(w io.Writer, res *mcp.CallToolResult) {
	for _, c := range res.Content {
		switch c := c.(type) {
		case mcp.TextContent:
			fmt.Fprintln(w, strings.TrimSuffix(c.Text, "\n"))
		case mcp.ImageContent:
			fmt.Fprintf(w, "[image %s, %d bytes base64]\n", c.MIMEType, len(c.Data))
		case mcp.AudioContent:
			fmt.Fprintf(w, "[audio %s, %d bytes base64]\n", c.MIMEType, len(c.Data))
		default:
			b, _ := json.Marshal(c)
			fmt.Fprintln(w, string(b))
		}
	}
	if len(res.Content) == 0 && res.StructuredContent != nil {
		b, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		fmt.Fprintln(w, string(b))
	}
}

func printJSON(v any) subcommands.ExitStatus {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fail(err)
	}
	fmt.Println(string(b))
	return subcommands.ExitSuccess
}
