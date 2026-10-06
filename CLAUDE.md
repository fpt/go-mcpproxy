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

mcpproxy is a stdio MCP server that proxies one upstream MCP server. The
upstream is either a local stdio server, restarted when its executable changes
so it can be rebuilt without restarting the AI agent, or a remote streamable
HTTP / SSE server, optionally OAuth-protected. mcpproxy also works as a
command-line MCP client (`tools`, `call`, `auth`).

- **mcpproxy/main.go**: Entry point using Google's subcommands pattern
- **internal/subcmd/**: `serve` (run the proxy); `tools`/`call` (CLI client, `client.go`); `auth`/`logout` (`auth.go`); `add`/`rm`/`ls`/`check` (allowlist); `help` comes from `subcommands.HelpCommand`. `target.go` turns a target (a URL, or a command after `--`) plus the `-transport`/`-header`/`-env` flags into an `app.Dialer`
- **internal/remote/**: Creates streamable HTTP / SSE clients, chooses the transport, and detects auth errors
- **internal/auth/**: Credential store (`store.go`, one 0600 file per normalized server URL, which implements mcp-go's `TokenStore`) and the OAuth authorization code + PKCE flow with a loopback callback (`flow.go`)
- **internal/mcptool/**: Mirrors upstream tools onto the mcp-go server (`SetTools` → `tools/list_changed`) and adds the built-in tools (`register.go`: `mcpproxy_restart`; `search.go`: `mcpproxy_search_tools`, `mcpproxy_call_tool`), configured by `mcptool.Options`
- **internal/app/**: `Upstream` supervisor over a `Dialer` (`dialer.go`: `StdioDialer`, `RemoteDialer`). Handles change detection (`fingerprint.go`), restart/launch/crash handling (`upstream.go`), argument validation against the running build's input schemas (`validate.go`), ToolSearch-style ranking (`search.go`), and the stderr tail buffer (`tailbuf.go`)
- **internal/allowlist/**: Allowlist matching (`allowlist.go`) and config editing (`config.go`, atomic save). Loaded from the user-level config (`~/.config/mcpproxy/config.json` or `$MCPPROXY_CONFIG`)
- **internal/authtest/**: In-process fake OAuth authorization server + protected MCP server (streamable HTTP and SSE) for tests
- **internal/apptest/**: Test helper that builds `internal/app/testdata/echoserver` with `-ldflags -X main.variant=...` to simulate rebuilds

### Key Behaviors
- `Upstream.CallTool` is the hot path. It stats the watched files, restarts the upstream if they changed or the process is down, checks that the tool exists, validates the arguments, and forwards the call. Failures become tool error results, never JSON-RPC errors.
- A failed restart stops the old process and keeps the last tool list. The next call retries.
- The allowlist (paths and URLs) is checked at startup and again before every (re)start or reconnect, for every subcommand that connects.
- When a call names an unknown tool or fails validation, the tools are re-listed once before the call is rejected. This handles remote servers whose tools changed without notifying the proxy.
- Auth errors drop the connection and carry a hint to run `mcpproxy auth <url>`. A `RemoteDialer` reloads stored credentials on every dial, so a running proxy picks up a new `auth` without restarting.
- The upstream is initialized with `mcp.LATEST_LEGACY_PROTOCOL_VERSION` for broad compatibility with servers under development.
