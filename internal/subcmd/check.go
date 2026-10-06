package subcmd

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/google/subcommands"
)

// CheckCmd reports whether a command is allowed by the allowlist.
type CheckCmd struct{}

func (*CheckCmd) Name() string     { return "check" }
func (*CheckCmd) Synopsis() string { return "Check whether a command is allowed to be proxied." }
func (*CheckCmd) Usage() string {
	return `check <command>:
  Show the config file in use, its allowlist patterns, and whether <command>
  resolves to an allowed executable.
`
}

func (*CheckCmd) SetFlags(*flag.FlagSet) {}

func (*CheckCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	allow, path, err := loadAllowlist()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	fmt.Printf("config: %s\n", path)
	patterns := allow.Patterns()
	if len(patterns) == 0 {
		fmt.Println("allow: (empty; every command is rejected)")
	}
	for _, p := range patterns {
		fmt.Printf("allow: %s\n", p)
	}
	if f.NArg() == 0 {
		return subcommands.ExitSuccess
	}

	resolved, err := allow.Resolve(f.Arg(0))
	if err != nil {
		fmt.Printf("rejected: %v\n", err)
		return subcommands.ExitFailure
	}
	fmt.Printf("allowed: %s\n", resolved)
	return subcommands.ExitSuccess
}
