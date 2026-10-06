package subcmd

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fpt/go-mcpproxy/internal/config"
	"github.com/fpt/go-mcpproxy/internal/hub"
	"github.com/fpt/go-mcpproxy/internal/mcptool"
	"github.com/google/subcommands"
	"github.com/mark3labs/mcp-go/server"
)

// Version is the mcpproxy version reported to clients and upstreams.
const Version = "0.1.0"

// ServeCmd runs mcpproxy as a stdio MCP server in front of the configured
// servers.
type ServeCmd struct {
	poll         time.Duration
	settle       time.Duration
	startTimeout time.Duration
	name         string
	debug        bool
	logFile      string
}

func (*ServeCmd) Name() string { return "serve" }
func (*ServeCmd) Synopsis() string {
	return "Serve the configured MCP servers over stdio, reloading on change."
}

func (*ServeCmd) Usage() string {
	return `serve [flags]:
  Run as a stdio MCP server that proxies every server in the config file
  (see "mcpproxy add"). Tools are exposed as "<server>__<tool>", next to the
  built-in tools mcpproxy_status, mcpproxy_restart, mcpproxy_search_tools and
  mcpproxy_call_tool.

  Command servers are restarted when their executable (or a watch file)
  changes. URL servers use credentials stored by "mcpproxy auth" and are
  reconnected when the connection or authorization fails. The config file
  itself is reloaded when it changes: servers are started, stopped or
  restarted, and the client is notified that the tool list changed.

`
}

func (p *ServeCmd) SetFlags(f *flag.FlagSet) {
	f.DurationVar(&p.poll, "poll", time.Second,
		"Interval for polling the config file and server executables (0: executables are "+
			"checked only on each call, and the config is not reloaded)")
	f.DurationVar(&p.settle, "settle", 300*time.Millisecond,
		"Watched files must be unchanged for this long before restarting")
	f.DurationVar(&p.startTimeout, "start-timeout", 30*time.Second,
		"Timeout for a server to connect, initialize and list its tools")
	f.StringVar(&p.name, "name", "mcpproxy", "Server name reported to the client")
	f.BoolVar(&p.debug, "debug", os.Getenv("DEBUG") != "", "Enable debug logging")
	f.StringVar(&p.logFile, "logfile", os.Getenv("LOGFILE"), "Log file path (default: stderr)")
}

func (p *ServeCmd) Execute(ctx context.Context, f *flag.FlagSet, _ ...any) subcommands.ExitStatus {
	if f.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "mcpproxy: serve takes no arguments; configure servers with "+
			"`mcpproxy add <name> -- <command> [args...]` or `mcpproxy add <name> <url>`")
		return subcommands.ExitUsageError
	}

	logger, closeLog, err := p.setupLogger()
	if err != nil {
		return fail(err)
	}
	defer closeLog()
	slog.SetDefault(logger)

	env, err := newEnvironment()
	if err != nil {
		return fail(err)
	}

	opts := env.hubOptions(os.Stderr)
	opts.Poll = p.poll
	opts.Settle = p.settle
	opts.StartTimeout = p.startTimeout
	opts.Logger = logger
	h := hub.New(opts)
	defer h.Close()

	srvOpts := []server.ServerOption{server.WithToolCapabilities(true), server.WithRecovery()}
	if p.debug {
		// Per-request logging is noisy; only enable it when debugging.
		srvOpts = append(srvOpts, server.WithLogger(logger))
	}
	s := server.NewMCPServer(p.name, Version, srvOpts...)
	mcptool.Register(s, h)

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A broken or missing config is not fatal: serve the built-in tools,
	// report the problem through mcpproxy_status, and pick up the fix.
	cfg, err := env.load()
	if err != nil {
		logger.Error("config not loaded; serving no servers until it is fixed", "error", err)
		h.SetConfigError(err)
		cfg = &config.Config{}
	}
	logger.Info("starting", "config", env.cfgPath, "servers", cfg.Names())
	h.Apply(ctx, cfg)

	if p.poll > 0 {
		go h.WatchConfig(ctx, env.cfgPath, p.poll)
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
