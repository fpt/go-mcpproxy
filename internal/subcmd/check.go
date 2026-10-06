package subcmd

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/fpt/go-mcpproxy/internal/remote"
	"github.com/google/subcommands"
)

// CheckCmd reports whether a command is allowed by the allowlist.
type CheckCmd struct{}

func (*CheckCmd) Name() string     { return "check" }
func (*CheckCmd) Synopsis() string { return "Check whether a command or URL is allowed." }
func (*CheckCmd) Usage() string {
	return `check [<command> | <url>]:
  Show the config file in use, its allowlist entries, and whether <command>
  resolves to an allowed executable (or <url> is an allowed server).
`
}

func (*CheckCmd) SetFlags(*flag.FlagSet) {}

func (*CheckCmd) Execute(_ context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	env, err := loadEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	fmt.Printf("config: %s\n", env.cfgPath)
	patterns := env.allow.Patterns()
	if len(patterns) == 0 {
		fmt.Println("allow: (empty; every command is rejected)")
	}
	for _, p := range patterns {
		fmt.Printf("allow: %s\n", p)
	}
	if f.NArg() == 0 {
		return subcommands.ExitSuccess
	}

	target := f.Arg(0)
	if remote.IsURL(target) {
		if err := env.allow.CheckURL(target); err != nil {
			fmt.Printf("rejected: %v\n", err)
			return subcommands.ExitFailure
		}
		fmt.Printf("allowed: %s\n", target)
		return subcommands.ExitSuccess
	}
	resolved, err := env.allow.Resolve(target)
	if err != nil {
		fmt.Printf("rejected: %v\n", err)
		return subcommands.ExitFailure
	}
	fmt.Printf("allowed: %s\n", resolved)
	return subcommands.ExitSuccess
}
