# harvester — a standalone Go web and document harvester

Cached handles and external gateway credentials outlive one process: migration changes must preserve both.

Where Vocabulary or Runtime disagrees with the code on a path, command or convention, follow the code and name each mismatch in your reply; `# Rules` binds regardless.

# Vocabulary

- binary: command dispatch · `cmd/harvester/`
- harvest: source resolution, fetching, search, conversion dispatch and cached handles · `internal/harvest/`
- conversion sidecar: pinned Python environment, OCR models and browser worker · `internal/harvestpy/`
- MCP: tool schemas, content blocks and authenticated gateway · `internal/harvestmcp/`
- read budget: one `read` answer's text stays within `output.maxResultBytes` UTF-8 bytes (default 50000), its items sharing it fairly after each one's `output.maxInlineChars`; `read` declares it to Claude Code as its `maxResultSizeChars` tool metadata · `internal/harvestmcp/service_render.go`
- offset: a `read` input paging one cached item; a cut item's `truncated:` line names the `offset` that continues it · `internal/harvest/cache.go`
- CLI: one-shot read, download, search and ask rendering; a read renders through MCP's `RenderReadItems`, under the same read budget · `internal/harvestcli/`
- ask: source questions through Claude or Codex executables · `internal/ask/`, `internal/engine/`
- configuration: `$HARVESTER_CONFIG`, else `${XDG_CONFIG_HOME:-$HOME/.config}/harvester/harvester.config.json` · `internal/config/`
- state: `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`, holding python, ask, cache and search; explicit cache and gateway state paths remain operator-owned · `internal/paths/`
- daemon: shared engine MCP service with a 0600 `{state}/daemon.sock`, exclusive `{state}/daemon.lock` and `daemon.log` in the log directory, `$PLUGIN_LOG_DIR/harvester`, else `{state}/logs` · `internal/daemon/`
- stdio proxy: `harvester mcp`, forwarding to daemon with session recovery and uncertain-delivery errors · `internal/daemon/proxy.go`
- managed search: the harvester's own SearXNG container `harvester-searxng`, created on a pinned image or adopted as found, when search is on, `search.managed` is not false and neither `search.searxngURL` nor `search.braveApiKey` is set; its facts live in `{state}/search/managed.json`, never in configuration · `internal/searxng/`
- setup: conversion provisioning, managed search and external gateway login service · `internal/setup/`
- doctor: binary, daemon, configuration, gateway, cache, search, output, tools, logs and conversion health rows; exit 1 on any warning · `internal/doctor/`
- testjail: isolated process homes and environment for test packages · `internal/testjail/`
- install count: `harvester mcp`'s anonymous once-per-UTC-day ping, its `{state}/install-id` and `{state}/install-ping-day` files and opt-outs `$HARVESTER_INSTALL_PING_OFFLINE`, `$DO_NOT_TRACK` · `internal/installping/`
- plugin: the installable manifests, launcher, release checksums and Claude Code commands · `plugins/harvester/`
- marketplace: the Claude Code and Codex plugin listings · `.claude-plugin/marketplace.json`, `.agents/plugins/marketplace.json`
- launcher: installs a checksum-verified versioned binary under `${HARVESTER_HOME:-$HOME/.local/share/harvester}` and maintains `{HARVESTER_HOME}/bin/harvester` · `plugins/harvester/libexec/harvester`
- release: version and checksum gates, reproducible assets · `scripts/release-check.sh`, `scripts/build-release.sh`
- CI: the pull-request checks and the publish on merge · `.github/workflows/ci.yml`, `.github/workflows/publish.yml`
- testing manual: isolation and validation commands · `docs/testing.md`

# Runtime

## Local

- Checks run on the host from the module root, testjail isolating every test package; narrowest: `go test ./internal/{package} -run {Test}`; while building: `go build ./... && go vet ./... && go test ./...`; done-gate: `make check`.
- `harvester serve` runs daemon in the foreground without an idle exit; stdio proxy starts it detached and replaces an outdated version using the installed binary.
- Every daemon start ensures managed search beside the socket, never delaying it; a backend turning ready changes the daemon's identity, so the next call any stdio proxy makes replaces it and sessions started after list `search_web`, while running sessions keep their tools; a boot check that finds the recorded container silent keeps its ready record (`KeepReady`).
- `harvester setup [--offline]` provisions conversion, then readies managed search (`--offline` never pulls; without Docker, one row and no exit-code change), then starts daemon; `external.enabled` also installs its login service and opens the authenticated gateway in that process.
- `harvester doctor [--json] [--skip-conversion]` prints the health rows; `harvester config show` names the search backend in effect on stderr.
- `harvester status [--json]` reports pid, version, uptime, endpoint and gateway state; `harvester stop` drains daemon.
- Release inputs: `scripts/release-check.sh`; checksum reproduction: `scripts/release-check.sh --release`.
- Build assets and refresh the launcher's checksum manifest: `scripts/build-release.sh --write-sums`.
- Release check: `make release-check`, after `git fetch origin main`, runs every check the release pull request runs: `make check` (the leak gate last), `make release`; `make release` fails on `develop` between releases by design (version not past main's).
- Scratch: `/tmp/harvester/{purpose}/`.

## CI

- `.github/workflows/ci.yml` on every pull request to `main`, never on a push: `check` (format, build, vet, tests, races, the OpenCode hook test, `scripts/release-check.sh`, the leak gate) on Linux and macOS and `release` (the version moved past main's, `scripts/release-check.sh --release`), each a `make` target, and `ci`, green only when every one succeeded; merging it runs `.github/workflows/publish.yml`: `publish` tags `v{version}` and publishes, then `verify` checks the published assets.

## Git

- Work lands on `develop`; `develop` is protected by the ruleset `develop keeps its history`: never force-push or delete it.
- `main` is protected by `main is release-only`: it moves only by merging the `develop` -> `main` pull request with a merge commit, after `ci` passes; never a direct push, never an admin override.
- Every pull request merged into `main` publishes: its version moves past main's, and a pre-release version (`-dev.N`) is bumped to the real release version in that pull request.
- Before opening it: `git fetch origin main`, then `make release-check` passes.
- After the merge, the `publish` job tags `v{version}` on the merge commit and creates the GitHub release; nobody pushes a tag by hand.

# Rules

## Data safety

- Preserve cached handles and gateway credentials through every state-format change: an older cache entry stays readable, and a stored credential is upgraded in place, never dropped.
- Run test packages through testjail and keep operator configuration, credentials and state outside fixtures.

## Interfaces

- Keep CLI and MCP content block shapes, tool schemas and error distinctions consistent when changing the shared harvest pipeline.
- Keep installable files under plugin; implementation and tests live outside it.
