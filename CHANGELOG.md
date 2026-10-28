# Changelog

Every release of harvester. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release.

## [Unreleased]

### Changed

- `read` no longer fits its answer to an estimated token budget: each item shows its content up to `output.maxInlineChars` characters, as before, and the whole answer goes back uncut. An answer over the client's MCP output limit is the client's to handle; Claude Code saves it to a file and gives the model the path. The `[cut: ~N of ~M estimated tokens shown …]` line is gone.
- The `include_content: false` size line reads `size: {chars} chars`, without the `~{n} tokens` estimate.
- `harvester --json` results no longer carry `tokens`, and new artifacts carry no `token_count` frontmatter; an older artifact's `token_count` line is ignored.
- The activity log writes through the shared plugin-log library: debug records go to their own `debug.jsonl` instead of `info.jsonl`, a level file past 20 MiB keeps up to five generations (`.1` to `.5`) instead of one, and a row that cannot be written is held and written first by the next write instead of being lost.
