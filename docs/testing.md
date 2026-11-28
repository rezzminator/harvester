# Testing

Run checks on the host from the Harvester module root, as CI runs them on its
Linux and macOS runners; `internal/testjail` gives every test package its own
home, state and environment, so no test reads the operator's configuration or
state.

From the Harvester module root, run:

```sh
go build ./...
go vet ./...
go test -json ./...
CGO_ENABLED=1 go test -race ./internal/harvest ./internal/harvestmcp
node --test plugins/harvester/opencode.test.mjs
```

The JSON test output records which tests ran and any skips. A skipped or
filtered test is a named gap, not a pass. The race check covers the concurrent
harvest and MCP packages. The Node test covers the OpenCode config hook without
installing a package or requiring a registry.

The full check, what done requires and what CI runs as `check`, is
`make check`: gofmt, then the build, vet and tests above, the race check, the
Node test, `scripts/release-check.sh`, and the leak scan of tracked files last
(`scripts/leak-check.sh`, its terms from `LEAK_TERMS`, else
`scripts/leak-terms.txt`). It stops at the first failing step, so a failure
names that step and leaves the later ones unrun.

## Managed search, by hand

Every Docker call in the tests goes through a fake runner, and
`internal/searxng` refuses the real Docker inside a test binary. To prove the
managed backend against the real Docker, run a second instance under a scratch
home, with its own container name and first port, so the operator's container
and state stay untouched:

```sh
scratch=/tmp/harvester/search-proof
export HOME=$scratch HARVESTER_HOME_DIR=$scratch XDG_CONFIG_HOME=$scratch/.config \
  XDG_STATE_HOME=$scratch/.local/state XDG_CACHE_HOME=$scratch/.cache \
  PLUGIN_LOG_DIR= HARVESTER_INSTALL_PING_OFFLINE=1 \
  HARVESTER_SEARXNG_CONTAINER=harvester-searxng-proof HARVESTER_SEARXNG_PORT=18888
harvester serve > $scratch/serve.out 2>&1 &   # the pinned image already present
# wait until "ready": true in $XDG_STATE_HOME/harvester/search/managed.json
harvester doctor --skip-conversion
harvester stop
docker rm -f -v harvester-searxng-proof
```

The daemon's start readies search on its own; `harvester setup` reaches its
search stage only after the conversion environment is healthy, so a scratch
home without one proves search through `serve`.

`HARVESTER_SEARXNG_CONTAINER` and `HARVESTER_SEARXNG_PORT` exist for this
proof; they are not operator configuration.
