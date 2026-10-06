# mcpproxy

An MCP server that proxies other MCP servers, mostly for MCP server
development.

When you develop an MCP server, your AI agent keeps running the build that was
current when the session started, so every rebuild means restarting the agent.
Register mcpproxy with the agent once, and list your servers in mcpproxy's
config. mcpproxy then restarts a server under the hood when its binary
changes, and picks up config edits while it runs.

```bash
make install
mcpproxy add godev -- ~/src/go-dev-mcp/output/godevmcp serve   # a local stdio server
mcpproxy add example https://mcp.example.com/mcp               # a remote HTTP/SSE server
mcpproxy auth example                                          # OAuth in the browser, if needed
claude mcp add mcpproxy -- mcpproxy serve
```

## Features

- **Several servers in one proxy.** `mcpproxy serve` serves every server in
  `~/.config/mcpproxy/config.toml`. Their tools are named
  `<server>__<tool>`, e.g. `godev__search_godoc`.
- **Config hot reload.** Editing the config, by hand or with `mcpproxy add` /
  `mcpproxy rm`, starts, stops or restarts the affected servers while the
  agent keeps running. The agent is sent `tools/list_changed`.
- **Restart on rebuild.** Each local server's executable, and any `watch`
  files, are checked before every call (one `stat` each) and polled in the
  background. After a rebuild, the new build is started once the files have
  stopped changing.
- **Validation against the running build.** Arguments are validated against
  the input schema of the build that is running now. A call shaped for an old
  build fails fast, and the error includes the current schema.
- **Helpful failures.** Start failures and crashes are reported with the
  server's recent stderr. `mcpproxy_status` shows every server's state.
- **Remote servers.** Streamable HTTP and SSE servers are supported, with
  OAuth (`mcpproxy auth`) or static headers. Header values can reference
  environment variables (`${TOKEN}`), which keeps secrets out of the config.
- **Tool search for rebuilt tools.** `mcpproxy_search_tools` and
  `mcpproxy_call_tool` reach tools that the client's own tool search hasn't
  indexed or holds outdated schemas for.
- **Command-line client.** `mcpproxy tools` and `mcpproxy call` list and call
  tools of any configured server.
- **The config is the allowlist.** mcpproxy only launches or contacts servers
  listed in the user-level config, outside any project.

The proxy forwards tools only. Prompts and resources are not proxied.

## Configuration

```toml
# ~/.config/mcpproxy/config.toml (or $MCPPROXY_CONFIG)
[servers.godev]
command = "/Users/me/src/go-dev-mcp/output/godevmcp"
args = ["serve"]
watch = ["/Users/me/src/go-dev-mcp/config.yaml"]   # optional extra files
env = { LOG_LEVEL = "debug" }                       # optional

[servers.example]
url = "https://mcp.example.com/mcp"
# transport = "sse"                                 # default: sse if the path ends in /sse
headers = { Authorization = "Bearer ${EXAMPLE_TOKEN}" }
```

## Commands

| Command | Purpose |
| --- | --- |
| `serve` | Run as a stdio MCP server for every configured server |
| `add <name> -- <command> [args...]` / `add <name> <url>` | Add or replace a server |
| `rm <name>...` | Remove servers |
| `ls [-v]` | List servers |
| `tools [<name>...]` | List tools |
| `call <name> <tool> [args...]` | Call a tool (`key=value`, `key:=json`, a JSON object, or `-` for stdin) |
| `auth <name>` / `logout <name>` | Store or delete OAuth credentials for a URL server |

See [doc/USAGE.md](doc/USAGE.md) for the details.

## Development

```bash
make build   # output/mcpproxy
make test    # unit + end-to-end tests (builds a fixture server with `go build`)
make lint
```
