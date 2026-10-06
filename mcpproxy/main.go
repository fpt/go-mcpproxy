package main

import (
	"context"
	"flag"
	"os"

	"github.com/fpt/go-mcpproxy/internal/subcmd"
	"github.com/google/subcommands"
)

func main() {
	subcommands.Register(subcommands.HelpCommand(), "help")
	subcommands.Register(subcommands.FlagsCommand(), "help")
	subcommands.Register(subcommands.CommandsCommand(), "help")
	subcommands.Register(&subcmd.ServeCmd{}, "proxy")
	subcommands.Register(&subcmd.ToolsCmd{}, "client")
	subcommands.Register(&subcmd.CallCmd{}, "client")
	subcommands.Register(&subcmd.AuthCmd{}, "client")
	subcommands.Register(&subcmd.LogoutCmd{}, "client")
	subcommands.Register(&subcmd.AddCmd{}, "config")
	subcommands.Register(&subcmd.RmCmd{}, "config")
	subcommands.Register(&subcmd.LsCmd{}, "config")
	subcommands.Register(&subcmd.WrapCmd{}, "config")

	flag.Parse()
	ctx := context.Background()
	os.Exit(int(subcommands.Execute(ctx)))
}
