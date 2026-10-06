package subcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/google/subcommands"
)

// AddCmd adds or replaces a server in the config.
type AddCmd struct {
	env       stringList
	watch     stringList
	transport string
	headers   stringList
}

func (*AddCmd) Name() string     { return "add" }
func (*AddCmd) Synopsis() string { return "Add (or replace) an MCP server." }
func (*AddCmd) Usage() string {
	return `add [flags] <name> -- <command> [args...]
add [flags] <name> <url>:
  Add a backend MCP server to the config, or replace the one with that name.
  Only servers in the config are ever launched or contacted. A running
  "mcpproxy serve" picks the change up automatically; its tools appear to the
  agent as "<name>__<tool>".

  <command> is stored as an absolute path (a bare name is looked up in PATH);
  it does not have to exist yet. <url> is a streamable HTTP or SSE endpoint.

  Examples:
    mcpproxy add godev -- ./output/godevmcp serve
    mcpproxy add -watch ./config.yaml myserver -- ./bin/myserver
    mcpproxy add example https://mcp.example.com/mcp
    mcpproxy add -header 'Authorization: Bearer ${API_TOKEN}' api https://api.example.com/mcp

`
}

func (p *AddCmd) SetFlags(f *flag.FlagSet) {
	f.Var(
		&p.env,
		"env",
		"KEY=VALUE for a command server's environment; ${NAME} expands (repeatable)",
	)
	f.Var(&p.watch, "watch", "Extra file whose change restarts a command server (repeatable)")
	f.StringVar(&p.transport, "transport", "",
		"Transport of a URL server: http or sse (default: sse if the path ends in /sse, else http)")
	f.Var(&p.headers, "header",
		`HTTP header "Name: value" for a URL server; ${NAME} expands at connect time (repeatable)`)
}

func (p *AddCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() < 2 {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	name, target := f.Arg(0), f.Args()[1:]
	if target[0] == "--" {
		target = target[1:]
	}
	if len(target) == 0 {
		return fail(errors.New("missing command or URL"))
	}
	if err := config.ValidateName(name); err != nil {
		return fail(err)
	}
	srv, err := p.server(target)
	if err != nil {
		return fail(err)
	}

	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	cfg, err := env.load()
	if err != nil {
		return fail(err)
	}
	_, existed := cfg.Servers[name]
	cfg.Servers[name] = srv
	if err := cfg.Save(env.cfgPath); err != nil {
		return fail(err)
	}
	verb := "added"
	if existed {
		verb = "updated"
	}
	fmt.Printf("%s: %s\t%s\n", verb, name, srv.String())
	if !srv.IsRemote() {
		if _, err := os.Stat(srv.Command); errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "note: %s does not exist yet\n", srv.Command)
		}
	}
	return subcommands.ExitSuccess
}

func (p *AddCmd) server(target []string) (config.Server, error) {
	if remote.IsURL(target[0]) {
		if len(target) > 1 {
			return config.Server{}, fmt.Errorf("unexpected arguments after URL: %q", target[1:])
		}
		if len(p.env) > 0 || len(p.watch) > 0 {
			return config.Server{}, errors.New("-env and -watch apply only to command servers")
		}
		srv := config.Server{URL: target[0], Transport: p.transport}
		for _, h := range p.headers {
			name, value, err := remote.ParseHeader(h)
			if err != nil {
				return config.Server{}, err
			}
			if srv.Headers == nil {
				srv.Headers = map[string]string{}
			}
			srv.Headers[name] = value
		}
		return srv, srv.Validate()
	}

	if p.transport != "" || len(p.headers) > 0 {
		return config.Server{}, errors.New("-transport and -header apply only to URL servers")
	}
	cmd, err := config.ResolveCommand(target[0])
	if err != nil {
		return config.Server{}, err
	}
	srv := config.Server{Command: cmd, Args: target[1:]}
	for _, kv := range p.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return config.Server{}, fmt.Errorf("-env %q must be KEY=VALUE", kv)
		}
		if srv.Env == nil {
			srv.Env = map[string]string{}
		}
		srv.Env[k] = v
	}
	for _, w := range p.watch {
		abs, err := config.AbsPath(w)
		if err != nil {
			return config.Server{}, err
		}
		srv.Watch = append(srv.Watch, abs)
	}
	return srv, srv.Validate()
}

// RmCmd removes servers from the config.
type RmCmd struct{}

func (*RmCmd) Name() string     { return "rm" }
func (*RmCmd) Synopsis() string { return "Remove MCP servers or wrapper tools." }
func (*RmCmd) Usage() string {
	return `rm <name>...:
  Remove servers or wrapper tools from the config. A running "mcpproxy serve"
  applies the change automatically. Stored OAuth credentials are kept; see "mcpproxy logout".
`
}

func (*RmCmd) SetFlags(*flag.FlagSet) {}

func (p *RmCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		fmt.Fprint(os.Stderr, p.Usage())
		return subcommands.ExitUsageError
	}
	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	cfg, err := env.load()
	if err != nil {
		return fail(err)
	}
	for _, name := range f.Args() {
		_, isServer := cfg.Servers[name]
		_, isWrapper := cfg.Wrapper[name]
		if !isServer && !isWrapper {
			return fail(fmt.Errorf("no server or wrapper named %q", name))
		}
		delete(cfg.Servers, name)
		delete(cfg.Wrapper, name)
	}
	if err := cfg.Save(env.cfgPath); err != nil {
		return fail(err)
	}
	for _, name := range f.Args() {
		fmt.Printf("removed: %s\n", name)
	}
	return subcommands.ExitSuccess
}

// LsCmd lists the configured servers.
type LsCmd struct {
	verbose bool
}

func (*LsCmd) Name() string     { return "ls" }
func (*LsCmd) Synopsis() string { return "List MCP servers and wrapper tools." }
func (*LsCmd) Usage() string {
	return `ls [-v]:
  Print the configured servers, one per line: name, then command or URL,
  followed by the wrapper tools: name, then "wrapper: <shell command>".
`
}

func (p *LsCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.verbose, "v", false,
		"Also show the config path, missing executables and stored credentials")
}

func (p *LsCmd) Execute(context.Context, *flag.FlagSet, ...any) subcommands.ExitStatus {
	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}
	if p.verbose {
		fmt.Printf("# config: %s\n", env.cfgPath)
	}
	cfg, err := env.load()
	if err != nil {
		return fail(err)
	}
	for _, name := range cfg.Names() {
		srv := cfg.Servers[name]
		line := name + "\t" + srv.String()
		if p.verbose {
			line += p.note(env, srv)
		}
		fmt.Println(line)
	}
	for _, name := range cfg.WrapperNames() {
		w := cfg.Wrapper[name]
		line := name + "\twrapper: " + w.Command
		if p.verbose && w.Dir != "" {
			line += "\t(in " + w.Dir + ")"
		}
		fmt.Println(line)
	}
	return subcommands.ExitSuccess
}

func (p *LsCmd) note(env *environment, srv config.Server) string {
	if !srv.IsRemote() {
		if _, err := os.Stat(srv.Command); err != nil {
			return "\t(missing)"
		}
		return ""
	}
	if c, err := env.creds.Load(srv.URL); err == nil && c != nil {
		return "\t(authorized)"
	}
	return ""
}

func fail(err error) subcommands.ExitStatus {
	fmt.Fprintln(os.Stderr, "mcpproxy:", err)
	return subcommands.ExitFailure
}
