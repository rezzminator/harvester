# Harvester

[![rezzminator/harvester MCP server](https://glama.ai/mcp/servers/rezzminator/harvester/badges/score.svg)](https://glama.ai/mcp/servers/rezzminator/harvester)

## Contents

- [Commands](#commands)
  - [Read output](#read-output)
- [Install and register](#install-and-register)
- [Web search](#web-search)
- [Configuration and state](#configuration-and-state)
- [Development](#development)

Harvester is a standalone Go plugin for fetching web and document content,
converting files, searching scholarly and web sources, and exposing the same
tools through MCP. Its source packages live in `internal/harvest`,
`internal/harvestmcp`, `internal/harvestpy`, and `internal/harvestcli`.

## Commands

The `harvester` executable supports these entry points:

- `harvester [flags] <source>...` reads URLs, files, and publication identifiers.
- `harvester download-file ...` downloads source files without parsing them.
- `harvester ask ...` reads the requested sources and asks the configured
  Claude or Codex CLI to answer.
- `harvester search [--type any|paper|book] ...` searches available sources.
- `harvester mcp` exposes the Harvester tools over stdio and proxies to the
  per-user `harvester serve` daemon.
- `harvester serve` hosts the persistent Harvester services and optional
  authenticated external MCP gateway.
- `harvester setup [--offline]` provisions conversion support, sets up web
  search (see [Web search](#web-search)) and refreshes the daemon.
- `harvester doctor [--json] [--skip-conversion]` checks the whole harvester,
  one row per part: configuration, the binary and its stable link, the daemon,
  the external gateway, the cache, search (the backend in effect, Docker, the
  managed container and a live JSON search), the output limits, the tools a
  new session lists, the log directory, conversion and the browser. A row that
  needs action is a warning naming its fix in `hint=`; any warning makes the
  exit code 1, none makes it 0.
- `harvester config path|show` prints the config path or effective Harvester
  settings, `show` naming the search backend in effect on stderr;
  `harvester version` prints the binary version.

In Claude Code the plugin adds `/harvester:doctor`: it runs the doctor through
the plugin's launcher, then proves the chat's own connection with one `read`
and, when the chat lists `search_web`, one web search, and reports each row,
every warning with its fix, and what it could not check.

The MCP server is named `harvester` in Claude Code, Codex, and OpenCode. Its
tools are `read`, `search_web`, `search_literature`, and `download_file`. Every
engine starts the plugin's `libexec/harvester mcp` launcher over stdio; the
launcher installs a checksum-verified binary on first use, and the proxy
connects to the per-user daemon over its private local endpoint. The daemon
stays available after clients exit.

Engine clients expose those tools under these names:

- **Claude Code:** `mcp__plugin_harvester_harvester__read`,
  `mcp__plugin_harvester_harvester__search_web`,
  `mcp__plugin_harvester_harvester__search_literature`,
  `mcp__plugin_harvester_harvester__download_file`
- **Codex:** `mcp__harvester__read`, `mcp__harvester__search_literature`,
  `mcp__harvester__download_file`, and `mcp__harvester__search_web` when a
  search backend can answer (see [Web search](#web-search)).
- **OpenCode:** `harvester_read`, `harvester_search_web`,
  `harvester_search_literature`, `harvester_download_file`

### Read output

`read` answers one text block per item. Each item shows its content up to
`output.maxInlineChars` characters (default 50,000), and the whole answer,
every block's header, status lines and content together, stays within
`output.maxResultBytes` UTF-8 bytes (default 50,000). When the items do not
fit together, each gets an equal share of what the headers leave, a short
item handing what it does not use to the longer ones, so a short item shows
whole; a cut never splits a character. Only an answer whose headers alone
pass the budget goes over it: each item then shows its header and a `size:`
line, no content, and the advice to read fewer items per call.

A cut item carries one line naming what it withheld and how to fetch it:
`truncated: chars {start}-{end} of {total} shown; read this item alone with
offset {end} to continue, or read the full text at the path`, the path
clause on a local server only. The path is the artifact's absolute path on the
block's second line, where a caller with a file tool reads the rest in pages. A caller without one sends the same source
again with `offset` (characters to skip from the start of the content): the
page is served from the cache with no second fetch, `offset` takes exactly
one source per call, and an offset at or past the end is an error naming the
item's length. The CLI takes `--offset` and applies the same budget to
`--include-content`; its `--json` results carry `chars`, and `next_offset` on
a cut item. `include_content: false` replaces the content with a
`size: {chars} chars` line.

Harvester counts and estimates no tokens: bytes bound every client below at
once, and stay close to token cost across scripts. What each client does
with one long tool result, as its own documentation and code gave it on
2026-11-27; harvester tests none of these limits, and a client release may
change them:

| Client | Its limit on one tool result | Past the limit | With harvester |
| --- | --- | --- | --- |
| Claude Code | 50,000 characters, and 25,000 tokens (`MAX_MCP_OUTPUT_TOKENS`) | the result goes to a file; the model gets its path | `read` declares its budget as `_meta["anthropic/maxResultSizeChars"]`, which Claude Code's documentation says replaces both limits for that tool up to a ceiling of 500,000 characters (Claude Code 2.1.98 or later), so an answer within the budget stays inline; the declaration is the configured budget as it is, so a budget past 500,000 meets that ceiling and Claude Code's file |
| Codex CLI | 10,000 tokens by its model catalog's default (`tool_output_token_limit`); one maintainer has called the unit bytes | the result is cut | lower `output.maxResultBytes` to about 30,000 (about 10,000 if the unit is bytes), or raise `mcp_servers.harvester.tools.read.output_token_limit` |
| OpenCode | 51,200 bytes or 2,000 lines | the result is cut; the full output goes to a file | the default budget is within its byte cap; its 2,000-line cap is not budgeted, so a page of short lines may still be cut there |

The MCP specification sets no limit and no continuation for a tool call;
`offset` is harvester's own, after the reference fetch server's
`start_index`.

## Install and register

Install the native plugin for each engine you use:

- **Claude Code:**

  ```sh
  claude plugin marketplace add rezzminator/harvester
  claude plugin install harvester@professor
  ```

- **Codex:**

  ```sh
  codex plugin marketplace add rezzminator/harvester
  codex plugin add harvester@professor
  ```

- **OpenCode:** clone the repository, then install its plugin directory:

  ```sh
  git clone https://github.com/rezzminator/harvester
  cd harvester
  opencode plugin --global "file://$PWD/plugins/harvester"
  ```

The Claude plugin installs from this repository's own marketplace
(`.claude-plugin/marketplace.json`); the Codex marketplace, named `professor`, is
`.agents/plugins/marketplace.json`.
OpenCode loads the packaged plugin's config hook, which registers `mcp.harvester`
only when no explicit entry already exists. The plugin uses the same launcher
and `harvester mcp` command on all engines.

Codex registers MCP through `plugins/harvester/.codex-plugin/plugin.json`,
which points to `plugins/harvester/mcp.json`. The MCP entry runs the contained
`./libexec/harvester mcp` command with the installed plugin directory as its
working directory and forwards the configured Harvester, XDG, logging (including `PLUGIN_LOG_DIR`), and
proxy environment names.

For an engine without the plugin, build or install the standalone executable
and put `harvester` on `PATH`. To build from a checkout:

```sh
go build -o "$HOME/.local/bin/harvester" ./cmd/harvester
```

Register it as a local stdio server with the engine's native command:

- **Claude Code:** `claude mcp add harvester -- harvester mcp`
- **Codex:** `codex mcp add harvester -- harvester mcp`
- **OpenCode:** add this entry to `opencode.json` or `opencode.jsonc`:

  ```json
  {
    "mcp": {
      "harvester": {
        "type": "local",
        "command": ["harvester", "mcp"],
        "enabled": true
      }
    }
  }
  ```

These manual registrations invoke the same stdio proxy. Export any Harvester,
XDG, logging, or HTTP proxy settings the daemon should inherit before starting
the engine. For a manual Codex entry in `config.toml`, list inherited variable
names in `env_vars`; values come from Codex's environment.

`harvester serve` is the persistent per-user daemon. It serves local MCP over
the Unix socket `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/daemon.sock`
(mode `0600`) and owns retrieval, conversion, browser workers, and cache
services. Its singleton lock is
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/daemon.lock` (mode `0600`);
the daemon's captured output is `daemon.log` (mode `0600`) in the log directory described under Logging. A proxy starts
it detached on demand; it remains running after clients exit. It checks the
installed binary's version once per session and again on a daemon reconnect; it
also rechecks a binary replaced during startup. An older proxy accepts a newer
daemon and never downgrades it, and an older installed build never replaces a
newer running daemon. When a new version or configuration replaces the daemon,
the successor takes the socket at once and serves new calls; the calls the
replaced daemon already took finish on it, for up to 30 minutes.
`harvester setup` refreshes the daemon after setup to apply current
configuration. `harvester status [--json]` reports the daemon, and
`harvester stop` drains and stops it.

When `external.enabled` is set, the same daemon also serves the optional
authenticated gateway at `127.0.0.1:18378` by default. `harvester setup` then
installs a Harvester user service (systemd on Linux, LaunchAgent on macOS) so
the gateway is available at login. With the external gateway disabled, setup
installs no service unit; the stdio proxy starts the daemon when needed.
When installed through a native plugin, the service follows the plugin's stable
binary link so plugin version updates take effect without replacing the
service.

With Docker running, `harvester setup` also gives the harvester its own web
search backend, and every daemon start keeps it up; see
[Web search](#web-search).

## Web search

`search_web` needs a backend: the operator's SearXNG (`search.searxngURL`), a
Brave Search API key (`search.braveApiKey`), or the SearXNG the harvester runs
itself. The operator's `search.searxngURL` wins, then `search.braveApiKey`;
with neither set and `search.managed` not `false`, the harvester manages its
own through Docker:

- `harvester setup` adds a search stage after conversion. A container already
  named `harvester-searxng` that answers a JSON search on a loopback port is
  adopted as it is, never recreated or removed. Otherwise setup pulls
  `searxng/searxng` by the digest pinned in the binary (`--offline` never
  pulls) and creates `harvester-searxng`: published on `127.0.0.1` only, port
  8888 or another free loopback port when 8888 is taken, restart policy
  `unless-stopped`, every capability dropped but the three its entrypoint
  needs (`CHOWN`, `SETGID`, `SETUID`), its log capped at 1 MB, and a generated
  `settings.yml` (SearXNG's defaults, a random secret key, the limiter off, the
  `json` format on) mounted read-only. The backend is ready only once a real
  JSON search answers.
- Every daemon start runs the same check in the background, never delaying the
  socket: it starts a stopped container and provisions a missing one, logging to
  `daemon.log`. A harvester-made container on an older pin is recreated on the
  current one.
- Without Docker, setup prints one row naming what is missing and how to get
  search, and its exit code stays what conversion makes it; the daemon logs one
  row and carries on. `harvester doctor` names the gap.

`search_web` is listed exactly when a backend can answer: an operator backend,
or a managed one whose last check answered a JSON search. A session keeps the
tool list it started with; once the managed backend is ready, the next call any
session makes replaces the daemon, as a configuration change does, and every
session started after that lists `search_web`. A daemon start that finds the
recorded container on its address but gets no answer keeps the backend ready,
so a backend that answers on and off never makes daemons replace each other;
`harvester doctor` shows the failed search.

Queries leave this machine: SearXNG sends each one to public search engines,
the one-word check search (`searxng`) that setup, every daemon start and
`harvester doctor` send included, and Brave's API receives each with the key;
both see this machine's address.

`search.managed: false` makes the harvester provision nothing and ignore its
record; it removes no container, and `harvester doctor` names a leftover one
with the command that removes it (`docker rm -f harvester-searxng`).
`search.enabled: false` turns web search off.

## Configuration and state

The config path is `HARVESTER_CONFIG` when set, otherwise
`${XDG_CONFIG_HOME:-$HOME/.config}/harvester/harvester.config.json`. Harvester
keeps its conversion environment, ask state, cache, and OAuth state under
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`; explicit cache or gateway
state paths in the config remain in use.

`search.managed` (boolean, default `true`) lets the harvester run its own
SearXNG when no operator backend is set (see [Web search](#web-search)). That
backend's facts live in the harvester's own record,
`{state}/search/managed.json`: its URL, container, image, whether it was
adopted, whether its last check answered, and the last error. Its generated
`settings.yml` sits beside it. The harvester never writes
`harvester.config.json`.

### Logging

Harvester writes its activity log to `$PLUGIN_LOG_DIR/harvester/` when
`PLUGIN_LOG_DIR` is set, otherwise to `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/logs/`;
no Harvester variable or config key moves it, and the daemon's `daemon.log`
sits beside the level files. The directory is `0700`. A record goes to
`errors.jsonl`, `warnings.jsonl`, `info.jsonl` or `debug.jsonl` (`0600`) as one
JSON row whose keys are `ts`, `plugin`, `level`, `event`, `msg`, `where`,
`session` when known, and `detail`, at most 4096 bytes; the event is the
record's message, `msg` its `err` field when it has one, `where` its component
(else its command), and `detail` its other scrubbed fields. A level file past
20 MiB moves to `{file}.1`, older generations shifting up to `{file}.5`, and a
`log-capped` row goes to `warnings.jsonl`. A row that cannot be written is held
for the next write, which writes it first. The screen shows an error as one
line and, once per failing streak, that the log cannot be written and how many
rows are held; nothing else is printed. `HARVESTER_LOG_LEVEL` and `HARVESTER_LOG_COMPONENTS`
still choose which records are written. A log left by an earlier version
(`harvester.jsonl` and its `.lock`) is not read or removed.

## Install count

`harvester mcp` sends one anonymous ping per UTC day, in the background, to
`https://install-ping.rezzminator.workers.dev/v1/ping`, so the project can
count real installs. The ping carries exactly: a random id (a UUIDv4 generated
once and kept in `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/install-id`,
never derived from the machine or you), the product `harvester`, the version,
the OS and CPU architecture, an empty plugin list, and the event: `install`
until a ping is stored, `daily` after that. The day of the last
stored ping is kept beside the id in `install-ping-day`. The counter stores one
row per day and id; it never stores your IP address, user name or request
headers. The first run prints one line on stderr saying the count is on.

Set `HARVESTER_INSTALL_PING_OFFLINE=1` or `DO_NOT_TRACK=1` (any value but `0`
or `false`) to turn it off: nothing is sent and no file is created. Nothing is
sent under CI (`CI` set to anything but `0` or `false`, or
`GITHUB_ACTIONS=true`) or inside Harvester's test jail. A failed ping is
logged as a warning and not retried until the next `harvester mcp` start.

## Development

Run the build, vet, and test checks from the module root as described in
[Testing](docs/testing.md). `plugins/harvester/` contains the native manifests,
OpenCode config hook, and launcher; the launcher verifies release assets
against `SHA256SUMS` and installs the versioned binary under `HARVESTER_HOME`.
