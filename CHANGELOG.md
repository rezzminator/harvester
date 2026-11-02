# Changelog

Every release of harvester. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release.

## [Unreleased]

### Fixed

- A Claude `ask` no longer leaves a resumable chat behind: it runs with `--no-session-persistence`, so Claude Code saves no session for it, as Codex's `--ephemeral` already did.

## [0.2.2] — 2026-11-01

### Fixed

- **A harvest keeps what it got when one part fails.** Site extractors, `ask` and the MCP tools skip only the item that cannot be read and say what was left out, instead of failing the whole answer. A document the cache cannot save comes back whole and inline, marked as not saved to the cache.
- A conversion that yields no text keeps what the converter read about the document, and its error names it (author, title); a converter failure after the browser got past a site's wall reports the converter's own words once, under a lead that matches the final failure.
- Removing a cookie-consent overlay never removes an element holding over half the page's text.
- A Steam review count that cannot be read shows as unknown instead of 0, and a review without an id is kept; a Substack comments answer must belong to the post being read.
- A short page that merely mentions Cloudflare or a captcha is no longer reported as a challenge page unless the server answered with an error.
- `harvester setup` prints a warning and carries on when it cannot delete a superseded conversion environment.
- On macOS, a cache or temp directory reached through a link (`/var` to `/private/var`) works, the browser worker installs into the resolved environment path, and a daemon socket path too long for the system fails with a clear error instead of `invalid argument`.
- `harvester stop` also drains the second daemon that takes over when two sessions start at once, instead of waiting for it until the timeout.
- `ask` skips a Codex output item it cannot decode, with a warning, and keeps the earlier message as the answer.
- An `ask` on a busy machine, or over an engine that leaves a child process running after it answers, returns the answer instead of failing with `WaitDelay expired before I/O complete`.
- On macOS, the leak check (`make leak`) runs under the system bash.

## [0.2.1] — 2026-11-01

### Changed

- **Claude Code installs harvester from this repository again.** The repository root carries a Claude marketplace named `professor`, so `claude plugin marketplace add rezzminator/harvester` followed by `claude plugin install harvester@professor` installs it; an install made from the professor marketplace keeps working under the same `harvester@professor` name.
- Pages read from forums and Q&A sites convert to cleaner Markdown: tables keep their columns and separator row, a code block keeps its language tag (a fenced `bash` block stays `bash`), inline code holding a backtick is fenced so it stays intact, links that lead nowhere (`#top`, `javascript:`) show as plain text, and literal `*` and `[` in the page text are escaped so they no longer read as emphasis or links.
- Setting up the conversion runtime refuses an archive that writes a link through a linked parent directory to reach outside the install root.
- Updated network and compression dependencies.

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
