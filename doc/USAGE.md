# mcpproxy usage

This guide covers every mcpproxy command in detail. For a short overview, see
the [README](../README.md).

- [Concepts](#concepts)
- [Installation and setup](#installation-and-setup)
- [Configuration file](#configuration-file)
- [Managing servers: `add`, `rm`, `ls`](#managing-servers-add-rm-ls)
- [Wrapper tools: `wrap`](#wrapper-tools-wrap)
- [Running the proxy: `serve`](#running-the-proxy-serve)
- [Built-in tools](#built-in-tools)
- [Authentication: `auth` and `logout`](#authentication-auth-and-logout)
- [Command-line client: `tools` and `call`](#command-line-client-tools-and-call)
- [Messages the agent sees](#messages-the-agent-sees)
- [Troubleshooting](#troubleshooting)
- [Reference](#reference)

## Concepts

mcpproxy is a stdio MCP server that sits between an MCP client, such as Claude
Code, and any number of **backend servers**. You register mcpproxy with the
client once; the backends are listed in mcpproxy's own config file.

Each backend has a **name** and is one of two kinds:

| Kind | Configured with | Example |
| --- | --- | --- |
| Command | an executable and its arguments, speaking MCP over stdio | `command = "/abs/path/godevmcp"`, `args = ["serve"]` |
| URL | a streamable HTTP or SSE endpoint | `url = "https://mcp.example.com/mcp"` |

The agent sees each backend tool as `<name>__<tool>`; for example, the tool
`search_godoc` of the server `godev` becomes `godev__search_godoc`. mcpproxy
adds four built-in tools named `mcpproxy_*`.

The config can also define **wrapper tools**. A wrapper is a custom tool that
runs one fixed shell command, such as `make test`, and returns its exit code,
stdout and stderr.

The config file is the **allowlist**: mcpproxy only launches executables,
contacts URLs and runs shell commands that are listed in it. The file lives in your home directory,
not in a project, so a project's MCP configuration cannot make mcpproxy run
anything else.

## Installation and setup

```bash
go install github.com/fpt/go-mcpproxy/mcpproxy@latest   # or `make install` from a checkout

mcpproxy add godev -- ~/src/go-dev-mcp/output/godevmcp serve
claude mcp add mcpproxy -- mcpproxy serve
```

That's all the setup the client needs: `mcpproxy serve` takes no arguments.
Add, change and remove servers later with `mcpproxy add` / `mcpproxy rm`, or
by editing the config. A running proxy applies the changes without
restarting the agent.

Other clients use the same command. A project `.mcp.json`, or any client that
uses the `mcpServers` format:

```json
{ "mcpServers": { "mcpproxy": { "command": "mcpproxy", "args": ["serve"] } } }
```

VS Code (`settings.json`):

```json
"mcp": { "servers": { "mcpproxy": { "type": "stdio", "command": "mcpproxy", "args": ["serve"] } } }
```

Run `mcpproxy help` to list the commands and `mcpproxy help <command>` for
details on one command.

## Configuration file

The first of these that applies is used:

1. `$MCPPROXY_CONFIG`, a path to the TOML file
2. `$XDG_CONFIG_HOME/mcpproxy/config.toml`
3. `~/.config/mcpproxy/config.toml`

A missing file means no servers. A complete example:

```toml
[servers.godev]
command = "/Users/me/src/go-dev-mcp/output/godevmcp"
args = ["serve"]
env = { LOG_LEVEL = "debug", GITHUB_TOKEN = "${GITHUB_TOKEN}" }
watch = ["/Users/me/src/go-dev-mcp/config.yaml"]

[servers.example]
url = "https://mcp.example.com/mcp"

[servers.legacy]
url = "https://legacy.example.com/events"
transport = "sse"
headers = { Authorization = "Bearer ${LEGACY_TOKEN}" }
```

| Key | Applies to | Description |
| --- | --- | --- |
| `command` | command | Absolute path of the executable. It doesn't have to exist yet |
| `args` | command | Arguments for the executable |
| `env` | command | Extra environment variables. The server otherwise inherits mcpproxy's environment |
| `watch` | command | Extra absolute paths whose change restarts the server; the executable is always watched |
| `url` | URL | `http://` or `https://` endpoint |
| `transport` | URL | `http` (streamable HTTP) or `sse`. Default: `sse` if the URL path ends in `/sse`, otherwise `http` |
| `headers` | URL | HTTP headers sent with every request |

Wrapper tools are defined under `[wrapper.<name>]`; see
[Wrapper tools](#wrapper-tools-wrap) for their keys.

Rules:

- **One kind per server.** A server has either `command` or `url`, never both.
- **Names.** A name has up to 32 characters: letters, digits, `-` and `_`. It
  must start with a letter or digit, must not contain `__`, and must not
  start with `mcpproxy`. Servers and wrappers share one namespace.
- **Environment variables.** Values in `env` and `headers` may reference
  environment variables as `${NAME}`. They are expanded when the server is
  started or connected, so secrets can stay out of the file. A bare `$` is
  left alone.
- **Unknown keys are errors**, so a typo such as `comand` is reported instead
  of being ignored.
- **Rewrites.** `mcpproxy add` and `mcpproxy rm` rewrite the file. Comments
  you wrote by hand are not preserved. The file is written atomically with
  mode `0600`, since it may contain headers.

## Managing servers: `add`, `rm`, `ls`

### `mcpproxy add [flags] <name> -- <command> [args...]`

### `mcpproxy add [flags] <name> <url>`

Adds a server, or replaces the one with the same name. The output says
`added` or `updated`.

```bash
mcpproxy add godev -- ./output/godevmcp serve          # stored as an absolute path
mcpproxy add tool -- my-installed-mcp-server           # a bare name is looked up in PATH
mcpproxy add -watch ./config.yaml -env DEBUG=1 dev -- ./bin/dev-server
mcpproxy add example https://mcp.example.com/mcp
mcpproxy add -transport sse legacy https://legacy.example.com/events
mcpproxy add -header 'Authorization: Bearer ${TOKEN}' api https://api.example.com/mcp
```

| Flag | Applies to | Description |
| --- | --- | --- |
| `-env KEY=VALUE` | command | Environment variable (repeatable) |
| `-watch path` | command | Extra file to watch (repeatable); stored as an absolute path |
| `-transport http\|sse` | URL | Transport, if the default guess is wrong |
| `-header "Name: value"` | URL | HTTP header (repeatable) |

- **Paths.** The command is stored as an absolute path, without resolving
  symlinks. A symlink retargeted by your build is therefore followed on the
  next restart.
- **Not built yet.** You can add a command that doesn't exist yet; `add`
  prints a note.
- **Quoting.** Put single quotes around values that contain `${...}`, so your
  shell doesn't expand them into the file.
- **Flag order.** Flags go before the name. Everything after `--` belongs to
  the server's command line.

### `mcpproxy rm <name>...`

Removes servers. If any name is unknown, nothing is removed. Stored OAuth
credentials are kept; delete them with `mcpproxy logout <name>` first if you
want them gone.

### `mcpproxy ls [-v]`

Prints one server per line: the name, a tab, then the command line or URL.
With `-v`, it also prints the config path, marks executables that don't exist
as `(missing)`, and marks URL servers with stored credentials as
`(authorized)`.

```
$ mcpproxy ls -v
# config: /Users/me/.config/mcpproxy/config.toml
example	https://mcp.example.com/mcp	(authorized)
godev	/Users/me/src/go-dev-mcp/output/godevmcp serve
```

## Wrapper tools: `wrap`

A wrapper tool runs one fixed shell command and reports how it went. It lets
the agent run your project's usual commands, such as tests, linters or a
build, through MCP. You decide the exact command line; the agent can't pass
arguments, so it can't run anything else through the tool.

```toml
[wrapper.test]
command = "make test"

[wrapper.lint]
command = "golangci-lint run ./..."
description = "Run the linters. Fix every reported issue."
dir = "~/src/app"
env = { GOFLAGS = "-mod=mod", TOKEN = "${CI_TOKEN}" }
timeout = "5m"
max_output = 20000
```

| Key | Default | Description |
| --- | --- | --- |
| `command` (required) | | Shell command, run with `/bin/sh -c` (`cmd /C` on Windows) |
| `description` | ``Run `<command>` in <dir>.`` | What the agent is told the tool does. A note about the result is always appended |
| `dir` | mcpproxy's working directory | Working directory. `~/` and `${NAME}` expand; a relative path is relative to mcpproxy's working directory |
| `env` | | Extra environment variables; values may use `${NAME}` |
| `timeout` | `10m` | Kill the command after this long. Go duration syntax: `90s`, `5m`, `1h30m` |
| `max_output` | `100000` | Bytes kept from the **end** of stdout and of stderr each |

**Working directory.** When Claude Code starts mcpproxy, its working
directory is the project directory. So `command = "make test"` runs the
current project's tests, as long as mcpproxy is registered per project, which
is the default scope of `claude mcp add`. Set `dir` to always use one
directory.

### The tool's result

The tool takes no arguments. Its result has structured content matching a
declared output schema, and the same JSON as text:

```json
{
  "exit_code": 2,
  "stdout": "ok   pkg/a  0.3s\nFAIL pkg/b ...",
  "stderr": "make: *** [test] Error 1\n"
}
```

- `exit_code` is the command's exit status. It is `-1` when the command did
  not exit by itself: it was killed by the timeout or a signal, or it could
  not be started. `error` then says why.
- `stdout` and `stderr` are captured separately, and only the last
  `max_output` bytes of each are kept. The end of the output is usually where
  failures are summarized. When something was dropped, `stdout_truncated` or
  `stderr_truncated` is `true`.
- `error` and the `*_truncated` fields appear only when they apply.
- The result is marked as an error (`isError: true`) whenever `exit_code` is
  not 0.
- Stdin is empty.
- On timeout or cancellation, the whole process group is killed, including
  children such as compilers started by `make`. Nothing keeps running in the
  background.

### `mcpproxy wrap [flags] <name> '<shell command>'`

### `mcpproxy wrap [flags] <name> -- <command> [args...]`

Adds a wrapper, or replaces the one with that name.

```bash
mcpproxy wrap test 'make test'
mcpproxy wrap -d 'Run the linters' -timeout 5m lint -- golangci-lint run ./...
mcpproxy wrap -dir ~/src/app build 'npm run build 2>&1 | tail -50'
```

**Giving the command**

- A single argument is used as the shell command as is, so pipes, `&&` and
  redirections work. Quote it.
- After `--`, the words are quoted for the shell and joined.
  `-- printf '%s|' 'a b'` becomes `printf '%s|' 'a b'`.

| Flag | Description |
| --- | --- |
| `-d text` | Description shown to the agent |
| `-dir path` | Working directory, stored as an absolute path |
| `-env KEY=VALUE` | Environment variable (repeatable) |
| `-timeout d` | Timeout (default 10m) |
| `-max-output n` | Bytes kept from the end of each stream (default 100000) |

`mcpproxy rm <name>` removes a wrapper, and `mcpproxy ls` lists wrappers
after the servers as `<name>\twrapper: <command>`. A running `mcpproxy serve`
applies all of these changes immediately.

### Trying a wrapper from the terminal

```bash
mcpproxy call test          # stdout and stderr pass through; exits with the command's exit code
mcpproxy call -json test    # prints the result JSON the agent would get
```

## Running the proxy: `serve`

```bash
mcpproxy serve [flags]
```

`serve` speaks MCP on stdin and stdout and logs to stderr. The MCP client
launches it; you don't normally run it by hand.

| Flag | Default | Description |
| --- | --- | --- |
| `-poll` | `1s` | Polling interval for the config file and the servers' watched files. With `0`, the config is not reloaded and executables are only checked on each call |
| `-settle` | `300ms` | Watched files must be unchanged for this long before a restart |
| `-start-timeout` | `30s` | Time limit for a server to connect, initialize and list its tools |
| `-name` | `mcpproxy` | Server name reported to the client |
| `-debug` | `$DEBUG` set | Debug logging, including one log line per MCP request |
| `-logfile path` | `$LOGFILE` | Write logs to a file instead of stderr |

### Startup

All configured servers are started in parallel, and `serve` answers the client
once each one has started or failed. A server that fails to start, or whose
executable doesn't exist yet, doesn't stop the others. Its error is shown by
`mcpproxy_status`, and it is retried on its next call or rebuild.

If the config file can't be parsed, `serve` still starts, with only the
built-in tools. `mcpproxy_status` reports the error, and the servers appear
once the file is fixed.

### Config hot reload

`serve` checks the config file every `-poll` interval. When the file changes:

| Change | Effect |
| --- | --- |
| Server added | It is started, and its tools are added |
| Server removed | It is stopped, and its tools are removed |
| Server changed (any key) | It is stopped and started again with the new settings |
| Server unchanged | Untouched; running calls continue |
| Wrapper added, changed or removed | The tool list is updated; the next call uses the new definition |
| File invalid | Nothing changes. `mcpproxy_status` shows the error until the file is valid again |

After each change, the client is sent `notifications/tools/list_changed`.

### Command servers: restart on rebuild

mcpproxy watches each server's executable and its `watch` files, recording
each file's size and modification time. The executable's path is watched
without resolving symlinks, so retargeting a symlink also counts as a change.
It checks for changes in two places:

- Before every call to that server, with one `stat` per file.
- In the background every `-poll` interval, so the tool list is current even
  while the agent is idle.

When a change is detected, mcpproxy does the following:

1. It waits until the files have been unchanged for `-settle`, giving up after
   30 seconds.
2. It starts the new process, initializes it and lists its tools.
3. It stops the old process.
4. If the tools changed, it sends `tools/list_changed`.

| Situation | Behavior |
| --- | --- |
| New build fails to start | The old process is stopped. The last known tools stay listed. Calls return the error plus the server's last 4 KB of stderr. The next call or rebuild retries |
| Server crashes during a call | The call returns an error with the recent stderr. The next call starts the server again |
| Rebuild while a call is still running | The running call is cut off when the old process stops |

The servers' stderr is passed through to mcpproxy's stderr, which your client
keeps in its MCP server log.

### URL servers

- **Credentials.** Credentials stored by `mcpproxy auth` are used, and tokens
  are refreshed automatically. They are reloaded on every reconnect, so
  running `auth` while the proxy is up takes effect on the next call.
- **Auth failures.** If authorization fails, the connection is dropped and the
  agent is told to have the user run `mcpproxy auth <name>`.
- **Lost connections.** If a call fails and the server stops responding, the
  connection is dropped and the next call reconnects. The failed call is not
  retried.
- **Changed tools.** When a call names an unknown tool or fails validation,
  the tool list is re-fetched once before the call is rejected. A server that
  reloads with new schemas therefore keeps working.

Toward every backend, mcpproxy introduces itself as `mcpproxy` and uses MCP
protocol version `2025-11-25`, the newest version with the initialize
handshake, for broad compatibility. Only tools are proxied; prompts and
resources are not.

## Built-in tools

Built-in tool names start with `mcpproxy_`, which server and wrapper names
may not use, so they never collide.

### `mcpproxy_status`

Shows every server, with its kind and target, whether it is running, its
tools, and its last error or recent stderr. It also lists the wrapper tools. It also shows a config error if
the file is invalid. Agents should use it when a proxied tool fails or is
missing.

```
[godev] /Users/me/src/go-dev-mcp/output/godevmcp serve
status: running (generation 3, started 2026-10-07T08:42:06+09:00)
tools: godev__read_godoc, godev__search_godoc, ...
```

### `mcpproxy_restart {"server": "<name>"}`

Restarts one command server, or reconnects one URL server, and reports its
status. Use it when the server depends on files that aren't watched.

### `mcpproxy_search_tools`

**Why it exists.** Clients cache tool definitions. Claude Code's ToolSearch,
for example, only indexes the tools it saw when it connected, so it cannot
find tools added or changed by a rebuild or a config change. This tool
searches the tools of all servers and wrappers as they are now, restarting any
server with a pending rebuild first.

| Parameter | Description |
| --- | --- |
| `query` (required) | One of the query forms below |
| `max_results` | Maximum number of keyword results (default 5; `0` means no limit) |

| Query | Meaning |
| --- | --- |
| `select:a__x,b__y` | Fetch these exact tools, in order; names that don't exist are reported |
| `keyword another` | Rank tools by the keywords |
| `+word keyword` | Require `word` in the tool name, then rank by the rest. `+godev__ search` searches only the server `godev` |
| *(empty)* | List all tools |

Scoring is case-insensitive. Each keyword adds points:

- 10 if it equals the full tool name
- 5 if it equals one word of the name (names are split on `_`, `-` and
  camelCase)
- 3 if it appears anywhere else in the name
- 2 if it appears in the title or description
- 1 if it appears in a parameter name

Results are full definitions inside `<function>` blocks. Servers that are not
running are listed as warnings.

```
1 of 1 matching tools (24 tools total)
<functions>
<function>{"name":"godev__search_godoc","description":"Search for Go package in pkg.go.dev","parameters":{...}}</function>
</functions>
```

### `mcpproxy_call_tool {"name": "<server>__<tool>", "arguments": {...}}`

Calls any backend tool by its full name, or a wrapper tool by its name, for tools the client has no
definition for or holds an outdated schema for. The call goes through the same
rebuild check and validation as a direct call.

## Authentication: `auth` and `logout`

### `mcpproxy auth [flags] <name>`

Runs the OAuth 2.1 authorization code flow with PKCE for a configured URL
server, then stores the credentials. The flow has these steps:

1. **Probe.** mcpproxy connects without credentials, using the server's
   configured headers. If the server accepts the connection, `auth` reports
   that no authorization is needed; `-force` runs the flow anyway. If the
   server answers 401, mcpproxy reads its protected resource metadata
   (RFC 9728) to find the authorization server.
2. **Client registration.** mcpproxy uses `-client-id` if given. Otherwise it
   reuses a stored registration if one exists, or registers itself
   dynamically (RFC 7591) as `mcpproxy`.
3. **Callback listener.** mcpproxy listens at
   `http://127.0.0.1:<port>/callback`.
4. **Browser.** mcpproxy opens the authorization URL in your browser and also
   prints it.
5. **Token exchange.** After you approve, mcpproxy checks `state` and
   exchanges the code (with the PKCE verifier) for tokens.
6. **Save and verify.** mcpproxy stores the credentials, connects with them,
   and prints the number of tools.

```
$ mcpproxy auth example
Registering mcpproxy with the authorization server...
Open this URL to authorize mcpproxy:

  https://auth.example.com/authorize?client_id=...&code_challenge=...

Waiting for authorization...
Authorized. example has 12 tools.
```

| Flag | Default | Description |
| --- | --- | --- |
| `-scope s` | | Scope to request (repeatable or comma-separated). If omitted, the stored scopes are reused |
| `-client-id`, `-client-secret` | | Use a pre-registered client instead of dynamic registration |
| `-port n` | auto | Callback port. Required when a pre-registered redirect URI has a fixed port |
| `-no-browser` | false | Print the URL instead of opening a browser |
| `-force` | false | Authorize even if the server accepts unauthenticated requests |
| `-timeout` | `5m` | How long to wait for approval |

**Pre-registered clients.** Register a client with the redirect URI
`http://127.0.0.1:<port>/callback`, then run
`mcpproxy auth -client-id ID -port <port> <name>`.

**Over SSH.** The callback listens on the machine running mcpproxy. Forward
the port and open the printed URL locally:

```bash
ssh -L 33418:127.0.0.1:33418 devbox
devbox$ mcpproxy auth -no-browser -port 33418 example
```

**Re-authorizing.** Running `auth` again reuses the stored client
registration while the same callback port is available.

### `mcpproxy logout <name>`

Deletes the stored client registration and tokens for a URL server. It exits
with status 1 if nothing was stored.

### Credential storage

- **Location.** Credentials live in a `credentials/` directory next to the
  config file, by default `~/.config/mcpproxy/credentials/`. The directory
  has mode `0700`. There is one `0600` JSON file per server URL, named by a
  hash of the normalized URL.
- **Keyed by URL.** Renaming a server keeps its credentials. Changing its URL
  needs a new `auth`.
- **Normalization.** The scheme and host are lowercased, and the fragment and
  trailing `/` are dropped; the query is kept.
- **Refresh.** Tokens refreshed by any mcpproxy process are written back.
- **Storage format.** Tokens are stored in plain files protected by file
  permissions, not in the OS keychain.

### Static tokens

For an API key or a long-lived token, configure a header instead of using
`auth`. Reference an environment variable so the secret isn't stored in the
file:

```bash
mcpproxy add -header 'Authorization: Bearer ${API_TOKEN}' api https://api.example.com/mcp
```

The variable must be set in mcpproxy's environment: the environment the MCP
client starts it with, or your shell for `tools` and `call`.

## Command-line client: `tools` and `call`

Both commands connect to configured servers by name, print the result and
exit. A command server's stderr is hidden unless you pass `-v`.

### `mcpproxy tools [flags] [<name>...]`

Lists the tools of the named servers, or of every server, using the names the
agent sees. Each line shows the tool name, a tab, and the first line of its
description.

```
$ mcpproxy tools godev
godev__get_github_content	Get content from GitHub with line-based paging for large files
...
```

| Flag | Description |
| --- | --- |
| `-q query` | Filter with the `mcpproxy_search_tools` query forms, with no result limit |
| `-json` | Print full definitions, including input schemas |
| `-v` | Show command servers' stderr |

If a server fails to connect, its error is printed, the other servers are
still listed, and the exit status is 1.

### `mcpproxy call [flags] <name> <tool> [arguments...]`

### `mcpproxy call [flags] <name>__<tool> [arguments...]`

```bash
mcpproxy call godev search_godoc query=mcp
mcpproxy call godev__search_godoc query=mcp            # the name the agent sees works too
mcpproxy call example search query='go mcp' limit:=5
echo '{"query": "x"}' | mcpproxy call example search -
```

There are four ways to give the tool's arguments:

| Form | Meaning | Example |
| --- | --- | --- |
| `key=value` | String value | `query='go mcp'` |
| `key:=json` | JSON value: number, boolean, null, array or object | `limit:=5`, `tags:='["a","b"]'` |
| one JSON object | The whole arguments object | `'{"query": "go", "limit": 5}'` |
| `-` | Read the arguments object from stdin | `... search -` |

The first two forms can be mixed. A value may itself contain `=` or `:=`.
Arguments are validated against the tool's schema before the call is sent.

| Flag | Description |
| --- | --- |
| `-json` | Print the raw result as JSON |
| `-timeout d` | Cancel the call after `d` |
| `-v` | Show a command server's stderr |

**Output.** Text content is printed as is. Images and audio are summarized;
use `-json` to get the data. Other content types are printed as JSON. If the
result has no content but has structured content, that is printed as JSON.
Tool errors go to stderr.

**Exit status**

| Status | Meaning |
| --- | --- |
| 0 | Success |
| 1 | The tool returned an error, validation failed, the server is unknown, or connecting or authorization failed |
| 2 | Usage error |

## Messages the agent sees

Problems are returned as tool error results (`isError: true`) starting with
`mcpproxy:`, so the agent can read them and react.

| Situation | Message (abridged) |
| --- | --- |
| Arguments don't match the current schema | `arguments for "godev__echo" do not match the tool's current input schema (it may have changed since you listed it): /: missing property 'text'` followed by the full current schema |
| Tool no longer exists | `tool "godev__old" does not exist on the upstream server (...). Available tools: godev__a, godev__b` |
| Build won't start | `upstream ... could not be (re)started: ...` followed by `upstream stderr:` and the panic or log output |
| Crash during a call | `call to upstream tool "godev__x" failed: ...` followed by the recent stderr |
| Authorization needed | ``... The server requires authorization. Run `mcpproxy auth example` in a terminal (ask the user to do so if you are an AI agent), then retry.`` |
| Unknown server in `mcpproxy_call_tool` | `no server named "x". Servers: example, godev` |

## Troubleshooting

**The client says the server failed to connect.** Run `mcpproxy serve`
yourself and check the first log lines; press Ctrl-D to exit. `serve` only
exits early on usage errors, such as passing arguments to it.

**A server's tools are missing.** Ask the agent to call `mcpproxy_status`, or
run `mcpproxy tools <name>` in a terminal. Either shows the start error and
the server's stderr.

**The agent doesn't see new tools after a rebuild or config change.** mcpproxy
sends `tools/list_changed`, but some clients ignore it. Use
`mcpproxy_search_tools` and `mcpproxy_call_tool`, which always see the
current tools.

**A config edit has no effect.** Check that the file is valid; an invalid file
is reported by `mcpproxy_status` and on mcpproxy's stderr. Also check that
`serve` wasn't started with `-poll 0`.

**A rebuild is picked up too early.** Increase `-settle` in the client's
registration, e.g. `claude mcp add mcpproxy -- mcpproxy serve -settle 1s`.

**`auth` fails with "dynamic client registration failed".** The provider needs
a pre-registered client; see `-client-id` and `-port`.

**A `${VAR}` header is empty.** The variable is not set in the environment
mcpproxy was started with. For Claude Code, set it with
`claude mcp add -e VAR=... mcpproxy -- mcpproxy serve`, or in the shell that
launches Claude Code.

## Reference

### Commands

| Command | Purpose |
| --- | --- |
| `serve [flags]` | Run the proxy as a stdio MCP server |
| `add [flags] <name> (-- <command> [args...] \| <url>)` | Add or replace a server |
| `wrap [flags] <name> ('<shell command>' \| -- <command> [args...])` | Add or replace a wrapper tool |
| `rm <name>...` | Remove servers or wrapper tools |
| `ls [-v]` | List servers and wrapper tools |
| `tools [flags] [<name>...]` | List tools |
| `call [flags] <name> <tool> [args...]` / `call <wrapper>` | Call a tool, or run a wrapper |
| `auth [flags] <name>` | Store OAuth credentials for a URL server |
| `logout <name>` | Delete stored credentials |
| `help [command]` | Show help |

### Environment variables

| Variable | Effect |
| --- | --- |
| `MCPPROXY_CONFIG` | Path of the config file. The `credentials/` directory is placed next to it |
| `XDG_CONFIG_HOME` | Base directory for the config when `MCPPROXY_CONFIG` is unset |
| `DEBUG` | If set, the default of `serve -debug` is true |
| `LOGFILE` | Default of `serve -logfile` |

### Files

| Path | Contents |
| --- | --- |
| `~/.config/mcpproxy/config.toml` | The servers (mode `0600` when written by mcpproxy) |
| `~/.config/mcpproxy/credentials/<hash>.json` | OAuth registration and tokens for one server URL (mode `0600`) |
