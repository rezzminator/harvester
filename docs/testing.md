# Testing

Run the Go checks inside the project fence. The standalone plugin module and
its testdata are the test boundary; do not run the project suite on the host.

From the fenced Harvester module root, run:

```sh
go build ./...
go vet ./...
go test -json ./...
```

The JSON test output records which tests ran and any skips. A skipped or
filtered test is a named gap, not a pass. The module packages are
`internal/harvest`, `internal/harvestmcp`, `internal/harvestpy`,
`internal/harvestcli`, the standalone CLI, and its support packages.
