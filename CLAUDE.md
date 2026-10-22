# harvester — a standalone Go web and document harvester

Cached handles and external gateway credentials outlive one process: migration changes must preserve both.

# Vocabulary

- binary: command dispatch · `cmd/harvester/`
- harvest: source resolution, fetching, search, conversion dispatch and cached handles · `internal/harvest/`
- conversion sidecar: pinned Python environment, OCR models and browser worker · `internal/harvestpy/`
- MCP: tool schemas, content blocks and authenticated gateway · `internal/harvestmcp/`
- CLI: one-shot read, download, search and ask rendering · `internal/harvestcli/`
- ask: source questions through Claude or Codex executables · `internal/ask/`, `internal/engine/`
- configuration: `$HARVESTER_CONFIG`, else `${XDG_CONFIG_HOME:-$HOME/.config}/harvester/harvester.config.json` · `internal/config/`
- state: `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`, holding python, ask and cache; explicit cache and gateway state paths remain operator-owned · `internal/paths/`
- daemon: shared engine MCP service with a 0600 `{state}/daemon.sock`, exclusive `{state}/daemon.lock` and `{state}/logs/daemon.log` · `internal/daemon/`
- stdio proxy: `harvester mcp`, forwarding to daemon with session recovery and uncertain-delivery errors · `internal/daemon/proxy.go`
- setup: conversion provisioning, legacy state migration and external gateway login service · `internal/setup/`
- doctor: configuration, cache, search and conversion health rows · `internal/doctor/`
- testjail: isolated process homes and environment for test packages · `internal/testjail/`
- plugin: the installable manifest, launcher and release checksums · `plugins/harvester/`
- launcher: installs a checksum-verified versioned binary under `${HARVESTER_HOME:-$HOME/.local/share/harvester}` and maintains `{HARVESTER_HOME}/bin/harvester` · `plugins/harvester/libexec/harvester`
- release: version and checksum gates, reproducible assets · `scripts/release-check.sh`, `scripts/build-release.sh`
- CI: build, vet, test and release workflows · `.github/workflows/`
- testing manual: isolation and validation commands · `docs/testing.md`

# Runtime

## Local

- Follow the testing manual for isolation; from the module root: `go build ./... && go vet ./... && go test ./...`.
- `harvester serve` runs daemon in the foreground without an idle exit; stdio proxy starts it detached and replaces an outdated version using the installed binary.
- `harvester setup [--offline]` provisions conversion and starts daemon; `external.enabled` also installs its login service and opens the authenticated gateway in that process.
- `harvester status [--json]` reports pid, version, uptime, endpoint and gateway state; `harvester stop` drains daemon.
- Release inputs: `scripts/release-check.sh`; checksum reproduction: `scripts/release-check.sh --release`.
- Build assets and refresh the launcher's checksum manifest: `scripts/build-release.sh --write-sums`.
- Scratch: `/tmp/harvester/{purpose}/`.

## CI

- `.github/workflows/ci.yml` checks pushes to `{develop|main}` and pull requests; `.github/workflows/release.yml` builds and publishes a `v*` tag from `main`.

# Rules

## Data safety

- Preserve cached handles and gateway credentials through state migration; remove a legacy source only after the destination is verified and its environment is healthy.
- Run test packages through testjail and keep operator configuration, credentials and state outside fixtures.

## Interfaces

- Keep CLI and MCP content block shapes, tool schemas and error distinctions consistent when changing the shared harvest pipeline.
- Keep installable files under plugin; implementation and tests live outside it.
