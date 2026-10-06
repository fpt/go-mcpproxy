# mcpproxy

An MCP server that proxies another MCP server during development. The
upstream can be a local stdio server, or a remote streamable HTTP or SSE
server, optionally protected by OAuth. mcpproxy can also list and call tools
from the command line.

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

- **Remote servers.** `mcpproxy serve <url>` proxies a streamable HTTP or SSE
  server over stdio. It uses credentials stored by `mcpproxy auth` and
  refreshes the token when it expires. If authorization fails, the agent is
  told to have the user run `mcpproxy auth <url>`. After that, the next call
  reconnects with the new credentials, without restarting the agent.
  mcpproxy can't watch a remote server's binary. Instead, when a call names an
  unknown tool or fails validation, the proxy re-lists the tools once before
  rejecting the call.

The proxy forwards tools only. Prompts and resources are not proxied.

## Install

```bash
make install   # go install ./mcpproxy
```

## Allowlist

mcpproxy only launches executables and connects to server URLs that are in
its allowlist. This applies to `serve`, `tools`, `call` and `auth`. Manage the
allowlist with:

```bash
mcpproxy add ./output/my-mcp-server            # relative paths are stored as absolute
mcpproxy add my-installed-server               # bare names are looked up in PATH
mcpproxy add '~/src/*/output/*'                # wildcards (quote them)
mcpproxy add '~/src/experiments/**'            # anything below a directory
mcpproxy add https://mcp.example.com/mcp       # a server URL (query and fragment are ignored)
mcpproxy add 'https://mcp.example.com/*'       # every URL with this prefix
mcpproxy ls                                    # list entries (-v: config path, missing files)
mcpproxy rm ./output/my-mcp-server             # remove an entry
mcpproxy check ./output/my-mcp-server          # would this command be allowed?
mcpproxy help                                  # all subcommands; `mcpproxy help <cmd>` for details
```

You can add an executable before it has been built. `add` and `rm` accept
several arguments; if any argument fails, the config is left unchanged.

The allowlist is stored in `~/.config/mcpproxy/config.json`. mcpproxy uses
`$XDG_CONFIG_HOME/mcpproxy/config.json` if that is set, or the path in
`$MCPPROXY_CONFIG`. The file can also be edited by hand:

```json
{
  "allow": [
    "/Users/me/src/my-mcp-server/output/my-mcp-server",
    "~/src/*/output/*"
  ]
}
```

- Entries are absolute paths (`~/` is expanded) with `filepath.Match`
  wildcards; a trailing `/**` matches anything below a directory.
- Commands are resolved via `PATH` and symlinks are evaluated before matching,
  so a symlink in an allowed directory pointing elsewhere is rejected.
- The check is repeated on every restart.
- A missing config file allows nothing.
- URL entries match the exact URL. A trailing `*` makes the entry a prefix
  match, so end it with `/*` to stay on one host.
- Avoid allowing interpreters such as `node` or `python`. Allowing an
  interpreter lets the proxy run any script.

## Usage

```bash
mcpproxy serve [flags] -- <command> [args...]   # local stdio server
mcpproxy serve [flags] <url>                    # remote streamable HTTP / SSE server
```

| Flag | Default | Description |
| --- | --- | --- |
| `-watch path` | | Extra file to watch (repeatable) |
| `-env KEY=VALUE` | | Extra environment variable for a stdio upstream (repeatable) |
| `-transport` | auto | `http` or `sse` for a URL; by default, `sse` if the path ends in `/sse` |
| `-header "Name: value"` | | HTTP header for a URL upstream, e.g. a static API token (repeatable) |
| `-poll` | `1s` | Background polling interval; `0` disables (changes are still detected per call) |
| `-settle` | `300ms` | How long watched files must be unchanged before restarting |
| `-start-timeout` | `30s` | Timeout for upstream initialize + tools/list |
| `-restart-tool` | `mcpproxy_restart` | Name of the built-in restart/status tool; empty disables it |
| `-search-tool` | `mcpproxy_search_tools` | Name of the built-in tool search; empty disables it |
| `-call-tool` | `mcpproxy_call_tool` | Name of the built-in call-by-name tool; empty disables it |
| `-name` | `mcpproxy:<command>` | Server name reported to the client |
| `-debug` / `-logfile` | | Debug logging / log to a file instead of stderr |

### Claude Code

```bash
claude mcp add my-server -- mcpproxy serve -- ~/src/my-mcp-server/output/my-mcp-server serve
```

Then run `make build` in your server's repository. The next tool call uses
the new build.

### `mcpproxy_search_tools` and `mcpproxy_call_tool`

Claude Code's built-in ToolSearch only indexes the tool definitions it
received when it connected. It does not find tools that a rebuild added or
changed behind the proxy, and the agent cannot call a tool whose definition it
never loaded.

- `mcpproxy_search_tools` searches the tools of the build that is running now.
  It first restarts the upstream if a rebuild is pending. Queries take the
  same forms as ToolSearch:
  - `select:a,b` fetches the named tools.
  - Plain keywords rank tools by matches in the name, description and
    parameter names.
  - `+word` requires `word` in the tool name.
  - An empty query lists all tools.

  Results are full definitions (name, description, parameter schema) inside
  `<functions>` blocks. `max_results` defaults to 5.
- `mcpproxy_call_tool {"name": ..., "arguments": {...}}` calls any upstream
  tool by name. It goes through the same restart-on-rebuild and schema checks
  as a direct call.

### Remote servers and authentication

```bash
mcpproxy add https://mcp.example.com/mcp
mcpproxy auth https://mcp.example.com/mcp      # opens the browser; stores the credentials
claude mcp add example -- mcpproxy serve https://mcp.example.com/mcp
```

`mcpproxy auth` runs the OAuth 2.1 authorization code flow with PKCE:

1. It discovers the authorization server from the server's 401 response
   (RFC 9728 protected resource metadata).
2. It registers mcpproxy dynamically, unless you pass `-client-id` (and
   `-client-secret`) for a pre-registered client.
3. It opens the browser and receives the callback on `127.0.0.1`.

Credentials (client registration and tokens) are stored per server URL in
`~/.config/mcpproxy/credentials/`. The directory has mode `0700` and each file
`0600`. Refreshed tokens are written back to the same file. Running
`mcpproxy auth` again reuses the stored registration.

| `auth` flag | Description |
| --- | --- |
| `-scope s` | Scope to request (repeatable or comma-separated) |
| `-client-id`, `-client-secret` | Use a pre-registered client instead of dynamic registration |
| `-port n` | Callback port, for a pre-registered redirect URI `http://127.0.0.1:<n>/callback` |
| `-no-browser` | Print the authorization URL instead of opening it |
| `-force` | Authorize even if the server accepts unauthenticated requests |

`mcpproxy logout <url>` deletes the stored credentials.

For a server that takes a static token, pass it as a header instead:
`-header "Authorization: Bearer $TOKEN"`.

### Command-line client

`tools` and `call` take the same targets as `serve`: a server URL, or a
command after `--`. They also take the same `-transport`, `-header` and
`-env` flags.

```bash
mcpproxy tools https://mcp.example.com/mcp                 # name and summary per line
mcpproxy tools -json -q 'select:search' https://mcp.example.com/mcp
mcpproxy tools -- ./output/my-server serve

mcpproxy call https://mcp.example.com/mcp search query='go mcp' limit:=5
mcpproxy call echo '{"message": "hi"}' -- ./output/my-server serve
echo '{"query": "x"}' | mcpproxy call https://mcp.example.com/mcp search -
```

Tool arguments can be given in three ways:

- `key=value` for a string.
- `key:=json` for a number, boolean, array, object or null.
- A single JSON object, or `-` to read one from stdin.

Arguments are checked against the tool's schema before the call is sent.
`call` exits with status 1 when the tool returns an error. Use `-json` to get
the raw result.

### `mcpproxy_restart`

The proxy adds a tool that forces a restart (or, for a URL, a reconnect) and reports the status (running
generation, tools, last error, recent stderr). Use it when the server depends
on files that mcpproxy does not watch, or to see why a build fails to start.

## Development

```bash
make build   # output/mcpproxy
make test    # unit + end-to-end tests (builds a fixture server with `go build`)
make lint
```
