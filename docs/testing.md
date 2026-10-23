# Testing

Run checks inside the project fence. The Harvester module and its testdata are
the Go test boundary; do not run the project suite on the host.

From the fenced Harvester module root, run:

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
