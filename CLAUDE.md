# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Essential Commands

```bash
make build      # Build to output/mcpproxy
make install    # Install to $GOPATH/bin/mcpproxy
make test       # Run all tests (end-to-end tests compile a fixture server with `go build`)
make test-race  # Run tests with the race detector
make fmt        # gofumpt + golangci-lint fmt
make lint       # golangci-lint
```

## Architecture Overview

mcpproxy is a stdio MCP server that proxies one upstream stdio MCP server and
restarts it when its executable changes, so that an MCP server under
development can be rebuilt without restarting the AI agent.

- **mcpproxy/main.go**: Entry point using Google's subcommands pattern
- **internal/subcmd/**: `serve` (run the proxy); `add`/`rm`/`ls`/`check` (manage and test the allowlist); `help` comes from `subcommands.HelpCommand`
- **internal/mcptool/**: Mirrors upstream tools onto the mcp-go server (`SetTools` → `tools/list_changed`) and adds the built-in tools (`register.go`: `mcpproxy_restart`; `search.go`: `mcpproxy_search_tools`, `mcpproxy_call_tool`), configured by `mcptool.Options`
- **internal/app/**: `Upstream` supervisor. Handles change detection (`fingerprint.go`), restart/launch/crash handling (`upstream.go`), argument validation against the running build's input schemas (`validate.go`), ToolSearch-style ranking (`search.go`), and the stderr tail buffer (`tailbuf.go`)
- **internal/allowlist/**: Allowlist matching (`allowlist.go`) and config editing (`config.go`, atomic save). Loaded from the user-level config (`~/.config/mcpproxy/config.json` or `$MCPPROXY_CONFIG`)
- **internal/apptest/**: Test helper that builds `internal/app/testdata/echoserver` with `-ldflags -X main.variant=...` to simulate rebuilds

### Key Behaviors
- `Upstream.CallTool` is the hot path. It stats the watched files, restarts the upstream if they changed or the process is down, checks that the tool exists, validates the arguments, and forwards the call. Failures become tool error results, never JSON-RPC errors.
- A failed restart stops the old process and keeps the last tool list. The next call retries.
- The allowlist is checked at startup and again before every (re)start.
- The upstream is initialized with `mcp.LATEST_LEGACY_PROTOCOL_VERSION` for broad compatibility with servers under development.
