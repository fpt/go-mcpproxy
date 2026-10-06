# mcpproxy

An MCP server that proxies another (stdio) MCP server during development.

When you develop an MCP server, the AI agent keeps running the build that was
current when the session started. Every rebuild means restarting the agent.
`mcpproxy` sits between the agent and your server and restarts your server
under the hood whenever its binary changes, so the agent session can stay
open.

## How it works

- **Restart on rebuild.** mcpproxy watches the upstream executable (plus any
  `-watch` files). It checks them in the background (`-poll`, default 1s) and
  again, cheaply (one `stat` per file), before every tool call. After a change
  it waits for the files to settle (`-settle`), starts the new build,
  initializes it, and re-lists its tools.
- **Live tool list.** The proxy re-exposes the upstream's tools. When a
  rebuild changes them, the proxy sends `notifications/tools/list_changed` so
  that clients supporting it can refresh their tool list.
- **Validation against the running build.** An agent may still call a tool
  with arguments shaped for an older build. Before forwarding, the proxy
  validates arguments against the input schema of the build that is running
  now. On a mismatch it returns a tool error that includes the current schema
  (or the list of available tools when the tool no longer exists). The agent
  can then retry without a round trip to a server that would reject the call.
- **Helpful failures.** If a build fails to start or crashes, the error
  returned to the agent includes the tail of the upstream's stderr (for
  example, the panic). The last known tools stay listed, and the next call
  after a fix starts the new build.
- **Allowlist.** mcpproxy only launches executables that match an allowlist
  in your user-level config file.

The proxy forwards tools only. Prompts and resources are not proxied.

## Install

```bash
make install   # go install ./mcpproxy
```

## Allowlist

Create `~/.config/mcpproxy/config.json`. mcpproxy uses
`$XDG_CONFIG_HOME/mcpproxy/config.json` if that is set, or the path in
`$MCPPROXY_CONFIG`.

```json
{
  "allow": [
    "~/src/my-mcp-server/output/my-mcp-server",
    "~/src/*/output/*",
    "~/src/experiments/**"
  ]
}
```

- Entries are absolute paths (`~/` is expanded) with `filepath.Match`
  wildcards; a trailing `/**` matches anything below a directory.
- Commands are resolved via `PATH` and symlinks are evaluated before matching,
  so a symlink in an allowed directory pointing elsewhere is rejected.
- The check is repeated on every restart.
- A missing config file allows nothing.
- Avoid allowing interpreters such as `node` or `python`. Allowing an
  interpreter lets the proxy run any script.

Check a command:

```bash
mcpproxy check ~/src/my-mcp-server/output/my-mcp-server
```

## Usage

```bash
mcpproxy serve [flags] -- <command> [args...]
```

| Flag | Default | Description |
| --- | --- | --- |
| `-watch path` | | Extra file to watch (repeatable) |
| `-env KEY=VALUE` | | Extra environment variable for the upstream (repeatable) |
| `-poll` | `1s` | Background polling interval; `0` disables (changes are still detected per call) |
| `-settle` | `300ms` | How long watched files must be unchanged before restarting |
| `-start-timeout` | `30s` | Timeout for upstream initialize + tools/list |
| `-restart-tool` | `mcpproxy_restart` | Name of the built-in restart/status tool; empty disables it |
| `-name` | `mcpproxy:<command>` | Server name reported to the client |
| `-debug` / `-logfile` | | Debug logging / log to a file instead of stderr |

### Claude Code

```bash
claude mcp add my-server -- mcpproxy serve -- ~/src/my-mcp-server/output/my-mcp-server serve
```

Then run `make build` in your server's repository. The next tool call uses
the new build.

### `mcpproxy_restart`

The proxy adds a tool that forces a restart and reports the status (running
generation, tools, last error, recent stderr). Use it when the server depends
on files that mcpproxy does not watch, or to see why a build fails to start.

## Development

```bash
make build   # output/mcpproxy
make test    # unit + end-to-end tests (builds a fixture server with `go build`)
make lint
```
