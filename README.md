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
  `mcp__harvester__download_file`; `mcp__harvester__search_web` is available
  when enabled in the Harvester config.
- **OpenCode:** `harvester_read`, `harvester_search_web`,
  `harvester_search_literature`, `harvester_download_file`

## Install and register

Install the native plugin for each engine you use:

- **Claude Code:**

  ```sh
  claude plugin marketplace add rezzminator/harvester
  claude plugin install harvester@harvester
  ```

- **Codex:**

  ```sh
  codex plugin marketplace add rezzminator/harvester
  codex plugin add harvester@harvester
  ```

- **OpenCode:** clone the repository, then install its plugin directory:

  ```sh
  git clone https://github.com/rezzminator/harvester
  cd harvester
  opencode plugin --global "file://$PWD/plugins/harvester"
  ```

The Claude and Codex plugin marketplaces are listed at the repository root.
OpenCode loads the packaged plugin's config hook, which registers `mcp.harvester`
only when no explicit entry already exists. The plugin uses the same launcher
and `harvester mcp` command on all engines.

Codex registers MCP through `plugins/harvester/.codex-plugin/plugin.json`,
which points to `plugins/harvester/mcp.json`. The MCP entry runs the contained
`./libexec/harvester mcp` command with the installed plugin directory as its
working directory and forwards the configured Harvester, XDG, logging, and
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
installs no service unit; the stdio proxy starts the daemon when needed.
When installed through a native plugin, the service follows the plugin's stable
binary link so plugin version updates take effect without replacing the
service.

## Configuration and state

The config path is `HARVESTER_CONFIG` when set, otherwise
`${XDG_CONFIG_HOME:-$HOME/.config}/harvester/harvester.config.json`. Harvester
keeps its conversion environment, ask state, cache, OAuth state, and logs under
`${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`. Setup can import Harvester
settings and state from their legacy locations; an existing plugin config and
explicit cache or gateway state paths remain in use. This import runs inside
Harvester and does not require PFM to install or configure the plugin.

Cross-filesystem migration verifies its copy and rescans for late writes; if the
legacy directory is retained and setup warns, it uses extra disk space, so stop
legacy writers and reconcile retained files before removing it manually. A
destination conflict never overwrites the published entry; setup retains the
legacy and destination entries for manual reconciliation.

## Development

Run the build, vet, and test checks inside the project fence as described in
[Testing](docs/testing.md). `plugins/harvester/` contains the native manifests,
OpenCode config hook, and launcher; the launcher verifies release assets
against `SHA256SUMS` and installs the versioned binary under `HARVESTER_HOME`.
