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

mcpproxy is a stdio MCP server (`mcpproxy serve`) that proxies every backend
MCP server listed in a user-level TOML config. Backends are local stdio
commands, restarted when their executable changes so they can be rebuilt
without restarting the AI agent, or remote streamable HTTP / SSE servers,
optionally OAuth-protected. Backend tools are exposed as `<server>__<tool>`.
The config is reloaded while running. mcpproxy also works as a command-line
MCP client (`tools`, `call`, `auth`).

- **mcpproxy/main.go**: Entry point using Google's subcommands pattern
- **internal/config/**: The TOML config (`~/.config/mcpproxy/config.toml` or `$MCPPROXY_CONFIG`). It loads with unknown keys rejected, validates, saves atomically with mode 0600, and expands `${NAME}`. The config is also the allowlist: only listed servers are ever launched or contacted
- **internal/hub/**: `Hub` owns one `app.Upstream` per configured server. `Apply` diffs a config and starts, stops or restarts backends; `WatchConfig` polls the file. `NewUpstream` builds an upstream from a `config.Server`, and is also used by the CLI
- **internal/mcptool/**: Publishes all backends' tools on the mcp-go server with the server-name prefix, republishing (and so sending `tools/list_changed`) only when the set actually changes. Also provides the built-in tools `mcpproxy_status`, `mcpproxy_restart` (`register.go`), `mcpproxy_search_tools` and `mcpproxy_call_tool` (`search.go`)
- **internal/app/**: `Upstream` supervisor for one backend over a `Dialer` (`dialer.go`: `StdioDialer`, `RemoteDialer`). Handles change detection (`fingerprint.go`), restart/launch/crash handling (`upstream.go`), argument validation against the running build's input schemas (`validate.go`), ToolSearch-style ranking (`search.go`), and the stderr tail buffer (`tailbuf.go`)
- **internal/remote/**: Creates streamable HTTP / SSE clients, chooses the transport, and detects auth errors
- **internal/auth/**: Credential store (`store.go`, one 0600 file per normalized server URL, which implements mcp-go's `TokenStore`) and the OAuth authorization code + PKCE flow with a loopback callback (`flow.go`)
- **internal/subcmd/**: `serve`; `add`/`rm`/`ls` (`servers.go`); `tools`/`call` (`client.go`); `auth`/`logout` (`auth.go`); config path and credential store (`env.go`)
- **internal/authtest/**: In-process fake OAuth authorization server + protected MCP server (streamable HTTP and SSE) for tests
- **internal/apptest/**: Test helper that builds `internal/app/testdata/echoserver` with `-ldflags -X main.variant=...` to simulate rebuilds

### Key Behaviors
- `Upstream.CallTool` is the hot path. It stats the watched files, restarts the backend if they changed or the process is down, checks that the tool exists, validates the arguments, and forwards the call. Failures become tool error results, never JSON-RPC errors. Messages use the prefixed tool names (`Options.ToolPrefix`).
- A failed restart stops the old process and keeps the last tool list. The next call retries.
- Config reload: a server whose definition changed in any way is stopped and started again. An invalid file keeps the current backends and is reported by `mcpproxy_status`. `serve` starts even with a broken or missing config.
- When a call names an unknown tool or fails validation, the tools are re-listed once before the call is rejected. This handles remote servers whose tools changed without notifying the proxy.
- Auth errors drop the connection and carry a hint to run `mcpproxy auth <name>`. A `RemoteDialer` reloads stored credentials on every dial.
- Backends are initialized with `mcp.LATEST_LEGACY_PROTOCOL_VERSION` for broad compatibility with servers under development.
