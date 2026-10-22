# Harvester

## Contents

- [Commands](#commands)
- [Install and register](#install-and-register)
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
- `harvester setup [--offline]` provisions conversion support and migrates
  legacy Harvester state.
- `harvester doctor [--json] [--skip-conversion]` checks Harvester's runtime
  and services.
- `harvester config path|show` prints the config path or effective Harvester
  settings; `harvester version` prints the binary version.

The MCP tool names and schemas remain `read`, `search_web`,
`search_literature`, and `download_file` under the `harvester-professor` server
identity. Every client starts `harvester mcp` over stdio; this agent-facing
proxy opens no listening port and connects to the per-user daemon over its
private local endpoint. The daemon stays available after clients exit. A Claude
marketplace plugin is an optional delivery and Claude integration path, not a
prerequisite for the executable or MCP server.

## Install and register

Harvester works with Claude Code, Codex, and OpenCode, either alongside PFM or
on its own. With PFM, enable `mcp.servers.harvester.enabled` and run `pfm install`;
PFM provisions the standalone `~/.local/bin/harvester` executable and registers
`harvester-professor` for the configured engine homes. This does
not depend on installing Harvester from Claude's marketplace.

For standalone use, build from an alpha checkout as shown below. When a stable
release is available, install its platform asset after verifying it against
the release's `SHA256SUMS`, then place it at `~/.local/bin/harvester`. Release
assets are named `harvester_{version}_{os}_{arch}`.

```sh
go build -o "$HOME/.local/bin/harvester" ./cmd/harvester
```

Ensure `~/.local/bin` exists and is on the account's PATH. Register that same
binary as a local stdio server in each engine you use:

- **Claude Code:** `claude mcp add --scope user harvester-professor -- "$HOME/.local/bin/harvester" mcp`
- **Codex:** add to `~/.codex/config.toml`:

  ```toml
  [mcp_servers.harvester-professor]
  command = "harvester"
  args = ["mcp"]
  env_vars = [
    "HARVESTER_HOME_DIR", "HARVESTER_CONFIG", "HARVESTER_PYTHON_ROOT",
    "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME",
    "HARVESTER_LOG_LEVEL", "HARVESTER_LOG_COMPONENTS", "HARVESTER_LOG_MIRROR",
    "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
  ]
  ```

- **OpenCode:** add to the user's `opencode.json` or `opencode.jsonc`:

  ```json
  {
    "mcp": {
      "harvester-professor": {
        "type": "local",
        "command": ["harvester", "mcp"],
        "enabled": true
      }
    }
  }
  ```

These registrations resolve `harvester` from PATH (normally
`~/.local/bin/harvester`) and invoke the same stdio proxy. To use the optional
Claude plugin delivery, install it from a published Harvester marketplace
listing when one is available; that route is not needed for PFM or standalone
registrations.

Codex `env_vars` forwards these variable names from the environment in which
Codex starts; the config stores names, not host values. Export any Harvester,
XDG, logging, or HTTP proxy settings the daemon should inherit before starting
Codex. `HARVESTER_HOME` is intentionally omitted because it configures the
optional plugin launcher, not the installed MCP binary.

`harvester serve` is the persistent per-user daemon. It serves local MCP over
the Unix socket `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/daemon.sock`
(mode `0600`) and owns retrieval, conversion, browser workers, and cache
services. Its singleton lock is
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/daemon.lock` (mode `0600`);
the daemon log is `.../harvester/logs/daemon.log` (mode `0600`). A proxy starts
it detached on demand; it remains running after clients exit. It checks the
installed binary's version once per session and again on a daemon reconnect; it
also rechecks a binary replaced during startup. An older proxy accepts a newer
daemon and never downgrades it.
`harvester setup` refreshes the daemon after setup to apply current
configuration. `harvester status [--json]` reports the daemon, and
`harvester stop` drains and stops it.

When `external.enabled` is set, the same daemon also serves the optional
authenticated gateway at `127.0.0.1:18378` by default. `harvester setup` then
installs a Harvester user service (systemd on Linux, LaunchAgent on macOS) so
the gateway is available at login. With the external gateway disabled, setup
installs no service unit; the stdio proxy starts the daemon when needed. This
service is separate from PFM's chat-only MCP daemon.

## Configuration and state

The config path is `HARVESTER_CONFIG` when set, otherwise
`${XDG_CONFIG_HOME:-$HOME/.config}/harvester/harvester.config.json`. Harvester
keeps its conversion environment, ask state, cache, OAuth state, and logs under
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`. Setup migrates gateway OAuth
state from `~/.professor/.harvester-state` to
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/.harvester-state` only when
both `cache.dir` and `external.stateDir` are unset; explicit locations remain
unchanged. An explicit `cache.dir` remains the cache location.

Cross-filesystem migration verifies its copy and rescans for late writes; if the
legacy directory is retained and setup warns, it uses extra disk space, so stop
legacy writers and reconcile retained files before removing it manually. A
destination conflict never overwrites the published entry; setup retains the
legacy and destination entries for manual reconciliation.

## Development

Run the build, vet, and test checks inside the project fence as described in
[Testing](docs/testing.md). The nested `plugins/harvester/` directory contains
the optional Claude plugin manifest and launcher; the launcher verifies
release assets against `SHA256SUMS` and installs the versioned binary under
`HARVESTER_HOME`.
