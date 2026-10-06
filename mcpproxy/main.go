package main

import (
	"context"
	"flag"
	"os"

	"github.com/fpt/go-mcpproxy/internal/subcmd"
	"github.com/google/subcommands"
)

func main() {
	subcommands.Register(subcommands.HelpCommand(), "")
	subcommands.Register(subcommands.FlagsCommand(), "")
	subcommands.Register(subcommands.CommandsCommand(), "")
	subcommands.Register(&subcmd.ServeCmd{}, "")
	subcommands.Register(&subcmd.CheckCmd{}, "")

	flag.Parse()
	ctx := context.Background()
	os.Exit(int(subcommands.Execute(ctx)))
}
