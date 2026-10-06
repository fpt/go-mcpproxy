package subcmd

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/hub"
	"github.com/fpt/go-mcpproxy/internal/wrapper"
	"github.com/google/subcommands"
	"github.com/mark3labs/mcp-go/mcp"
)

// connect starts the upstream of a configured server for one-shot use.
func connect(
	ctx context.Context,
	env *environment,
	name string,
	verbose bool,
) (*app.Upstream, error) {
	srv, err := env.server(name)
	if err != nil {
		return nil, err
	}
	stderr := io.Discard
	if verbose {
		stderr = os.Stderr
	}
	opts := env.hubOptions(stderr)
	opts.Settle = 0
	up, err := hub.NewUpstream(name, srv, opts)
	if err != nil {
		return nil, err
	}
	if err := up.Start(ctx); err != nil {
		_ = up.Close()
		return nil, err
	}
	return up, nil
}

// ToolsCmd lists the tools of configured servers.
type ToolsCmd struct {
	query   string
	json    bool
	verbose bool
}

func (*ToolsCmd) Name() string     { return "tools" }
func (*ToolsCmd) Synopsis() string { return "List the tools of MCP servers." }
func (*ToolsCmd) Usage() string {
	return `tools [flags] [<name>...]:
  List the tools of the named servers, or of every configured server, as
  "<server>__<tool>" (the names the agent sees through "mcpproxy serve").

`
}

func (p *ToolsCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.query, "q", "",
		`Search query, as for mcpproxy_search_tools ("select:a,b", keywords, "+word")`)
	f.BoolVar(&p.json, "json", false, "Print full tool definitions as JSON")
	f.BoolVar(&p.verbose, "v", false, "Show the stderr of command servers")
}

func (p *ToolsCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	cfg, err := env.load()
	if err != nil {
		return fail(err)
	}
	names := f.Args()
	if len(names) == 0 {
		names = append(cfg.Names(), cfg.WrapperNames()...)
		if len(names) == 0 {
			return fail(
				errors.New("nothing is configured (see `mcpproxy add` and `mcpproxy wrap`)"),
			)
		}
	}

	var tools []mcp.Tool
	status := subcommands.ExitSuccess
	for _, name := range names {
		if w, ok := cfg.Wrapper[name]; ok {
			tools = append(tools, wrapper.Tool(name, w))
			continue
		}
		up, err := connect(ctx, env, name, p.verbose)
		if err != nil {
			fmt.Fprintf(os.Stderr, "mcpproxy: %s: %v\n", name, err)
			status = subcommands.ExitFailure
			continue
		}
		for _, t := range up.Tools() {
			t.Name = name + config.ToolSeparator + t.Name
			tools = append(tools, t)
		}
		_ = up.Close()
	}

	if p.query != "" {
		tools = app.SearchTools(tools, p.query, 0).Tools
	}
	if p.json {
		if st := printJSON(tools); st != subcommands.ExitSuccess {
			return st
		}
		return status
	}
	for _, t := range tools {
		desc, _, _ := strings.Cut(strings.TrimSpace(t.Description), "\n")
		fmt.Printf("%s\t%s\n", t.Name, desc)
	}
	return status
}

// CallCmd calls a tool of a configured server.
type CallCmd struct {
	json    bool
	verbose bool
	timeout time.Duration
}

func (*CallCmd) Name() string     { return "call" }
func (*CallCmd) Synopsis() string { return "Call a tool of an MCP server." }
func (*CallCmd) Usage() string {
	return `call [flags] <name> <tool> [arguments...]
call [flags] <name>__<tool> [arguments...]
call [flags] <wrapper>:
  Call a tool of a configured server and print the result, or run a wrapper
  tool, passing its stdout and stderr through and exiting with its exit code. Arguments are
  checked against the tool's input schema before calling. The exit status is
  1 if the tool reports an error.

  Arguments are either a single JSON object, "-" to read a JSON object from
  stdin, or any number of:
    key=value     string value
    key:=json     JSON value (number, boolean, array, object, null)

  Examples:
    mcpproxy call godev search_godoc query=mcp
    mcpproxy call godev__search_godoc query=mcp
    mcpproxy call example search query='go mcp' limit:=5
    echo '{"query": "x"}' | mcpproxy call example search -

`
}

func (p *CallCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.json, "json", false, "Print the raw result as JSON")
	f.BoolVar(&p.verbose, "v", false, "Show the stderr of a command server")
	f.DurationVar(&p.timeout, "timeout", 0, "Timeout for the call (0: none)")
}

func (p *CallCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if st, ok := p.runWrapper(ctx, f.Args()); ok {
		return st
	}
	name, tool, rest, err := splitCallArgs(f.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcpproxy: %v\n\n", err)
		fmt.Fprint(os.Stderr, p.Usage())
		return subcommands.ExitUsageError
	}
	args, err := parseToolArguments(rest, os.Stdin)
	if err != nil {
		return fail(err)
	}

	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	up, err := connect(ctx, env, name, p.verbose)
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
	req.Params.Name = tool
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

// runWrapper runs args[0] if it names a wrapper tool, passing its stdout and
// stderr through and exiting with its exit code (1 if it did not exit).
// ok is false if args[0] is not a wrapper.
func (p *CallCmd) runWrapper(ctx context.Context, args []string) (subcommands.ExitStatus, bool) {
	if len(args) == 0 {
		return 0, false
	}
	env, err := newEnvironment()
	if err != nil {
		return fail(err), true
	}
	cfg, err := env.load()
	if err != nil {
		return fail(err), true
	}
	w, ok := cfg.Wrapper[args[0]]
	if !ok {
		return 0, false
	}
	if len(args) > 1 {
		return fail(fmt.Errorf("wrapper tool %q takes no arguments", args[0])), true
	}
	if p.timeout > 0 && (w.Timeout == 0 || p.timeout < w.Timeout) {
		w.Timeout = p.timeout
	}
	res := wrapper.Run(ctx, w)
	if p.json {
		if st := printJSON(res); st != subcommands.ExitSuccess {
			return st, true
		}
	} else {
		fmt.Print(res.Stdout)
		fmt.Fprint(os.Stderr, res.Stderr)
		if res.StdoutTruncated || res.StderrTruncated {
			fmt.Fprintln(os.Stderr, "mcpproxy: output was truncated (see max_output)")
		}
	}
	if res.Error != "" {
		fmt.Fprintln(os.Stderr, "mcpproxy:", res.Error)
	}
	if res.ExitCode < 0 {
		return subcommands.ExitFailure, true
	}
	return subcommands.ExitStatus(res.ExitCode), true
}

// splitCallArgs accepts "<name> <tool> args..." or "<name>__<tool> args...".
func splitCallArgs(args []string) (name, tool string, rest []string, err error) {
	if len(args) == 0 {
		return "", "", nil, errors.New("missing server name")
	}
	if n, t, ok := strings.Cut(args[0], config.ToolSeparator); ok && n != "" && t != "" {
		return n, t, args[1:], nil
	}
	if len(args) < 2 {
		return "", "", nil, errors.New("missing tool name")
	}
	return args[0], args[1], args[2:], nil
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
