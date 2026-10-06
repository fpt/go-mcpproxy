package subcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/google/subcommands"
)

// WrapCmd defines a wrapper tool that runs one shell command.
type WrapCmd struct {
	description string
	dir         string
	env         stringList
	timeout     time.Duration
	maxOutput   int
}

func (*WrapCmd) Name() string { return "wrap" }

func (*WrapCmd) Synopsis() string { return "Add (or replace) a wrapper tool running a shell command." }

func (*WrapCmd) Usage() string {
	return `wrap [flags] <name> '<shell command>'
wrap [flags] <name> -- <command> [args...]:
  Define a tool <name> that runs a fixed shell command (with "sh -c") and
  returns its exit code, stdout and stderr separately. The agent cannot pass
  arguments to it. A running "mcpproxy serve" picks the change up
  automatically. Equivalent config:

    [wrapper.<name>]
    command = "make test"

  Examples:
    mcpproxy wrap test 'make test'
    mcpproxy wrap -d 'Run the linters' -timeout 5m lint -- golangci-lint run ./...
    mcpproxy wrap -dir ~/src/app build 'npm run build'

`
}

func (p *WrapCmd) SetFlags(f *flag.FlagSet) {
	f.StringVar(&p.description, "d", "", "Description shown to the agent")
	f.StringVar(&p.dir, "dir", "",
		"Working directory, stored as an absolute path (default: mcpproxy's working directory, "+
			"which is the project directory for Claude Code)")
	f.Var(&p.env, "env", "KEY=VALUE added to the environment; ${NAME} expands (repeatable)")
	f.DurationVar(&p.timeout, "timeout", 0, "Kill the command after this long (default 10m)")
	f.IntVar(&p.maxOutput, "max-output", 0,
		"Bytes kept from the end of stdout and of stderr each (default 100000)")
}

func (p *WrapCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() < 2 {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	name, words := f.Arg(0), f.Args()[1:]
	var command string
	switch {
	case words[0] == "--" && len(words) > 1:
		command = shellJoin(words[1:])
	case words[0] != "--" && len(words) == 1:
		command = words[0]
	default:
		return fail(errors.New("give the command as one quoted string, or after --"))
	}
	if err := config.ValidateName(name); err != nil {
		return fail(err)
	}

	w := config.Wrapper{
		Command:     command,
		Description: p.description,
		Timeout:     p.timeout,
		MaxOutput:   p.maxOutput,
	}
	if p.dir != "" {
		dir, err := config.AbsPath(p.dir)
		if err != nil {
			return fail(err)
		}
		w.Dir = dir
	}
	for _, kv := range p.env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return fail(fmt.Errorf("-env %q must be KEY=VALUE", kv))
		}
		if w.Env == nil {
			w.Env = map[string]string{}
		}
		w.Env[k] = v
	}
	if err := w.Validate(); err != nil {
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
	if _, ok := cfg.Servers[name]; ok {
		return fail(fmt.Errorf("%q is already a server name", name))
	}
	_, existed := cfg.Wrapper[name]
	cfg.Wrapper[name] = w
	if err := cfg.Save(env.cfgPath); err != nil {
		return fail(err)
	}
	verb := "added"
	if existed {
		verb = "updated"
	}
	fmt.Printf("%s: %s\twrapper: %s\n", verb, name, command)
	return subcommands.ExitSuccess
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellJoin quotes words for sh so that "-- go test ./..." round-trips.
func shellJoin(words []string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		if shellSafe.MatchString(w) {
			quoted[i] = w
		} else {
			quoted[i] = "'" + strings.ReplaceAll(w, "'", `'\''`) + "'"
		}
	}
	return strings.Join(quoted, " ")
}
