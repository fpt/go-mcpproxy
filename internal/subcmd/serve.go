package subcmd

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fpt/go-mcpproxy/internal/app"
	"github.com/fpt/go-mcpproxy/internal/mcptool"
	"github.com/google/subcommands"
	"github.com/mark3labs/mcp-go/server"
)

// Version is the mcpproxy version reported to clients and upstreams.
const Version = "0.1.0"

// ServeCmd runs mcpproxy as a stdio MCP server in front of an upstream server.
type ServeCmd struct {
	target       targetFlags
	watch        stringList
	poll         time.Duration
	settle       time.Duration
	startTimeout time.Duration
	restartTool  string
	searchTool   string
	callTool     string
	name         string
	debug        bool
	logFile      string
}

func (*ServeCmd) Name() string     { return "serve" }
func (*ServeCmd) Synopsis() string { return "Proxy an MCP server, restarting it on rebuild." }
func (*ServeCmd) Usage() string {
	return `serve [flags] -- <command> [args...]
serve [flags] <url>:
  Proxy the tools of an upstream MCP server over stdio.

  With <command>, the upstream is a local stdio server; it is restarted when
  its executable (or a -watch file) changes. With <url>, the upstream is a
  streamable HTTP or SSE server; credentials stored by "mcpproxy auth" are
  used, and it is reconnected when the connection or authorization fails.

  The target must be in the allowlist (see "mcpproxy add").

`
}

func (p *ServeCmd) SetFlags(f *flag.FlagSet) {
	f.Var(&p.watch, "watch", "Extra file to watch for changes (repeatable)")
	p.target.setFlags(f)
	f.DurationVar(
		&p.poll,
		"poll",
		time.Second,
		"Interval for polling watched files in the background (0 disables; changes are still detected on each call)",
	)
	f.DurationVar(&p.settle, "settle", 300*time.Millisecond,
		"Watched files must be unchanged for this long before restarting")
	f.DurationVar(&p.startTimeout, "start-timeout", 30*time.Second,
		"Timeout for the upstream to initialize and list its tools")
	f.StringVar(&p.restartTool, "restart-tool", "mcpproxy_restart",
		"Name of the built-in restart/status tool (empty disables it)")
	f.StringVar(&p.searchTool, "search-tool", "mcpproxy_search_tools",
		"Name of the built-in tool search over the running build (empty disables it)")
	f.StringVar(&p.callTool, "call-tool", "mcpproxy_call_tool",
		"Name of the built-in tool that calls an upstream tool by name (empty disables it)")
	f.StringVar(
		&p.name,
		"name",
		"",
		"Server name reported to the client (default: mcpproxy:<command>)",
	)
	f.BoolVar(&p.debug, "debug", os.Getenv("DEBUG") != "", "Enable debug logging")
	f.StringVar(&p.logFile, "logfile", os.Getenv("LOGFILE"), "Log file path (default: stderr)")
}

func (p *ServeCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() == 0 {
		fmt.Fprint(os.Stderr, p.Usage())
		f.PrintDefaults()
		return subcommands.ExitUsageError
	}
	spec := f.Args()

	logger, closeLog, err := p.setupLogger()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return subcommands.ExitFailure
	}
	defer closeLog()
	slog.SetDefault(logger)

	env, err := loadEnvironment()
	if err != nil {
		logger.Error("load config", "error", err)
		return subcommands.ExitFailure
	}
	// Reject a disallowed target up front instead of serving an empty proxy.
	dialer, err := p.target.dialer(env, spec)
	if err != nil {
		logger.Error("invalid upstream", "error", err)
		return subcommands.ExitFailure
	}

	up, err := app.New(app.Options{
		Dialer:        dialer,
		Watch:         p.watch,
		Settle:        p.settle,
		StartTimeout:  p.startTimeout,
		Stderr:        os.Stderr,
		ClientName:    "mcpproxy",
		ClientVersion: Version,
		Logger:        logger,
	})
	if err != nil {
		logger.Error("configure upstream", "error", err)
		return subcommands.ExitFailure
	}

	name := p.name
	if name == "" {
		name = "mcpproxy:" + targetName(spec)
	}
	opts := []server.ServerOption{server.WithToolCapabilities(true), server.WithRecovery()}
	if p.debug {
		// Per-request logging is noisy; only enable it when debugging.
		opts = append(opts, server.WithLogger(logger))
	}
	s := server.NewMCPServer(name, Version, opts...)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A failed first start is not fatal: the developer may simply not have
	// built the server yet. The next rebuild or call retries.
	if err := up.Start(ctx); err != nil {
		logger.Warn("initial upstream start failed", "error", err)
	}
	defer func() { _ = up.Close() }()

	mcptool.Register(s, up, mcptool.Options{
		RestartTool: p.restartTool,
		SearchTool:  p.searchTool,
		CallTool:    p.callTool,
		ServerName:  targetName(spec),
	})

	if p.poll > 0 {
		go up.Watch(ctx, p.poll)
	}

	if err := server.ServeStdio(s); err != nil {
		logger.Error("server error", "error", err)
		return subcommands.ExitFailure
	}
	return subcommands.ExitSuccess
}

func (p *ServeCmd) setupLogger() (*slog.Logger, func(), error) {
	level := slog.LevelInfo
	if p.debug {
		level = slog.LevelDebug
	}
	opts := &slog.HandlerOptions{Level: level}
	if p.logFile == "" {
		// stdout carries the MCP JSON-RPC stream; logs must go to stderr.
		return slog.New(slog.NewTextHandler(os.Stderr, opts)), func() {}, nil
	}
	f, err := os.OpenFile(p.logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("open log file: %w", err)
	}
	return slog.New(slog.NewTextHandler(f, opts)), func() { _ = f.Close() }, nil
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}
