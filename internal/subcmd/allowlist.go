package subcmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/fpt/go-mcpproxy/internal/allowlist"
	"github.com/google/subcommands"
)

// AddCmd adds an executable (or pattern) to the allowlist.
type AddCmd struct{}

func (*AddCmd) Name() string     { return "add" }
func (*AddCmd) Synopsis() string { return "Allow an executable or server URL to be proxied." }
func (*AddCmd) Usage() string {
	return `add <executable|url>...:
  Add executables or server URLs to the allowlist. Paths are stored as absolute paths; a bare
  name is looked up in PATH. Wildcards are allowed (quote them), and a trailing
  "/**" allows everything below a directory, e.g.
    mcpproxy add ./output/my-server
    mcpproxy add '~/src/my-server/output/*'
    mcpproxy add https://mcp.example.com/mcp
    mcpproxy add 'https://mcp.example.com/*'
`
}

func (*AddCmd) SetFlags(*flag.FlagSet) {}

func (*AddCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		fmt.Fprint(os.Stderr, (&AddCmd{}).Usage())
		return subcommands.ExitUsageError
	}
	return editConfig(f.Args(), func(cfg *allowlist.Config, entry string) error {
		if !allowlist.IsPattern(entry) && !allowlist.IsURL(entry) {
			if _, err := os.Stat(entry); errors.Is(err, os.ErrNotExist) {
				fmt.Fprintf(os.Stderr, "note: %s does not exist yet\n", entry)
			}
		}
		if cfg.Add(entry) {
			fmt.Printf("added: %s\n", entry)
		} else {
			fmt.Printf("already allowed: %s\n", entry)
		}
		return nil
	})
}

// RmCmd removes an executable (or pattern) from the allowlist.
type RmCmd struct{}

func (*RmCmd) Name() string     { return "rm" }
func (*RmCmd) Synopsis() string { return "Remove an executable or server URL from the allowlist." }
func (*RmCmd) Usage() string {
	return `rm <executable|url>...:
  Remove entries from the allowlist. Give the path or pattern as shown by
  "mcpproxy ls" (relative paths and "~/" are expanded the same way as "add").
`
}

func (*RmCmd) SetFlags(*flag.FlagSet) {}

func (*RmCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		fmt.Fprint(os.Stderr, (&RmCmd{}).Usage())
		return subcommands.ExitUsageError
	}
	return editConfig(f.Args(), func(cfg *allowlist.Config, entry string) error {
		if !cfg.Remove(entry) {
			return fmt.Errorf("not in allowlist: %s", entry)
		}
		fmt.Printf("removed: %s\n", entry)
		return nil
	})
}

// editConfig applies edit to each argument and saves the config only if
// every argument succeeded.
func editConfig(
	args []string,
	edit func(cfg *allowlist.Config, entry string) error,
) subcommands.ExitStatus {
	path, err := allowlist.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	cfg, err := allowlist.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	for _, arg := range args {
		entry, err := allowlist.CanonicalEntry(arg)
		if err == nil {
			err = edit(cfg, entry)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return subcommands.ExitFailure
		}
	}
	if err := cfg.Save(path); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

// LsCmd lists the allowlist.
type LsCmd struct {
	verbose bool
}

func (*LsCmd) Name() string     { return "ls" }
func (*LsCmd) Synopsis() string { return "List allowed executables and server URLs." }
func (*LsCmd) Usage() string {
	return `ls [-v]:
  Print the allowlist entries, one per line.
`
}

func (p *LsCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&p.verbose, "v", false, "Also show the config file path and missing executables")
}

func (p *LsCmd) Execute(context.Context, *flag.FlagSet, ...any) subcommands.ExitStatus {
	path, err := allowlist.DefaultPath()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	cfg, err := allowlist.LoadConfig(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	if p.verbose {
		fmt.Printf("# config: %s\n", path)
	}
	for _, e := range cfg.Allow {
		if p.verbose && !allowlist.IsPattern(e) && !allowlist.IsURL(e) {
			if _, err := os.Stat(e); err != nil {
				fmt.Printf("%s\t(missing)\n", e)
				continue
			}
		}
		fmt.Println(e)
	}
	return subcommands.ExitSuccess
}
