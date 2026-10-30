# Changelog

Every release of harvester. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release.

## [0.2.0] — 2026-10-30

### Changed

- **Breaking: harvester installs as `harvester@professor`.** The Claude Code plugin installs from the professor marketplace; this repository's own Claude marketplace file is gone, so an install made as `harvester@harvester` gets no further updates. The Codex marketplace at the repository root is renamed `professor`. To migrate:

  ```sh
  claude plugin uninstall harvester@harvester
  claude plugin marketplace remove harvester
  claude plugin marketplace add rezzminator/professor
  claude plugin install harvester@professor
  ```

  On Codex, remove `harvester@harvester` and its marketplace, then `codex plugin marketplace add rezzminator/harvester` and `codex plugin add harvester@professor`.
- **Breaking: the activity log moves and the screen goes quiet.** It is written to `$PLUGIN_LOG_DIR/harvester/` when `PLUGIN_LOG_DIR` is set, otherwise to `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/logs/`, as `errors.jsonl`, `warnings.jsonl`, `info.jsonl` and `debug.jsonl` (`0600`, directory `0700`), one JSON row per record of at most 4096 bytes; the daemon's `daemon.log` sits beside them. A level file past 20 MiB keeps up to five generations (`.1` to `.5`), and a row that cannot be written is held and written first by the next write instead of being lost. The screen shows an error as one line and, once per failing streak, that the log cannot be written; nothing else is printed. `HARVESTER_LOG_MIRROR` is removed; `HARVESTER_LOG_LEVEL` and `HARVESTER_LOG_COMPONENTS` still choose which records are written. A log left by an earlier version (`harvester.jsonl` and its `.lock`) is neither read nor removed.
- **Breaking: `harvester setup` no longer imports state from legacy locations.** It provisions conversion and refreshes the daemon; config, cache, OAuth, ask and conversion state are read only from harvester's own paths (`HARVESTER_CONFIG` or `${XDG_CONFIG_HOME:-$HOME/.config}/harvester/`, and `${XDG_STATE_HOME:-$HOME/.local/state}/harvester/`), and explicit cache or gateway state paths in the config remain in use. State still at a legacy location is no longer moved: copy it into those paths by hand before upgrading.
- `read` no longer fits its answer to an estimated token budget: each item shows its content up to `output.maxInlineChars` characters, as before, and the whole answer goes back uncut. An answer over the client's MCP output limit is the client's to handle; Claude Code saves it to a file and gives the model the path. The `[cut: ~N of ~M estimated tokens shown …]` line is gone.
- The `include_content: false` size line reads `size: {chars} chars`, without the `~{n} tokens` estimate.
- `harvester --json` results no longer carry `tokens`, and new artifacts carry no `token_count` frontmatter; an older artifact's `token_count` line is ignored.
