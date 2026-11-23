# Changelog

Every release of harvester. Versions follow [semantic versioning](https://semver.org); each release is the `main` commit tagged `v<version>`, with a GitHub release.

## [Unreleased]

## [0.3.1] — 2026-11-23

### Added

- **A Reddit page the server cannot reach is read from the public archive.** Reddit refuses many server addresses outright: the page, `old.reddit.com`, the `.json` and `.rss` twins and the readers all answer HTTP 403. A Reddit address no live rung could read is now answered from the Arctic Shift archive of Reddit's posts and comments: a thread with its comment tree (or one comment's branch), a subreddit's listing (`top` ranked here by archived score), a search inside one subreddit, and a subreddit's about page. The result reads `via: reddit-archive` and is marked partial, naming the archive, the day of its record and what the answer leaves out; a call the archive sheds under load (HTTP 422, 429 or a server error) is asked again after 2 and then 5 seconds; when the archive cannot stand in, the failure says whether it could not be read or holds no record.

### Fixed

- A DOI with no open copy reads as `no_open_copy` again, with the advice to look for a preprint. When Google Scholar, the last fallback, answered a page without the DOI, the failure was labelled a failed connection and the caller was told to retry later.
- The Semantic Scholar source reads a paper record again: its `externalIds` object holds a number (`CorpusId`) the decoder refused, so the source failed on every DOI it knew.
- A failed read names the rungs it tried again (`Rungs tried: direct, chrome-impersonation, …`): the failure was worded twice, and the second wording, the one the caller reads, no longer had them.
- The launcher leaves a stable link that already names its version alone, and clears a staged link a dead launcher left under its own process id: an `mv` that kept its source stranded a `.harvester-link-{pid}` file in the binary home on every start.
- An `ask` over a long document gets the time it needs: its deadline adds one second per KiB of content to the 2 minutes plus 30 seconds per item, up to 10 minutes. A quoting question over a 130 KB paper takes about 170 seconds and was cut at 150.

## [0.3.0] — 2026-11-20

### Added

- **An anonymous install count.** `harvester mcp` sends one ping per UTC day, in the background and never on stdout, to the install counter at `install-ping.rezzminator.workers.dev`: a random install id, the version, the OS, the CPU architecture and the event (`install` until a ping is stored, `daily` after that). It is fire-and-forget with a 5 second timeout; two first starts racing share one id; the first run prints one line on stderr saying the count is on. `HARVESTER_INSTALL_PING_OFFLINE` or `DO_NOT_TRACK` opts out, and CI and test processes never send. The README's Install count section lists exactly what is sent.
- A `glama.json` maintainer manifest, so the Glama listing of harvester is claimed under this repository.

## [0.2.3] — 2026-11-14

### Fixed

- An `ask` no longer times out on a loaded machine: its deadline is 2 minutes plus 30 seconds per item read, up to 10 minutes, where a flat 60 seconds cut a third of asks over one or two items, and nearly all over five; a timeout names the deadline it ran out.
- A Claude `ask` boots lean: it starts no MCP server (each ask booted every configured server, the harvester's own among them), loads no skill and gets only the `Read` and `Grep` tools, so the engine reads only the prepared files the prompt names.
- A Claude `ask` no longer leaves a resumable chat behind: it runs with `--no-session-persistence`, so Claude Code saves no session for it, as Codex's `--ephemeral` already did.
- A Codex `ask` loads no plugin (`--disable plugins`), so an installed harvester plugin no longer starts an older harvester whose daemon replaced the one serving the ask, cutting it and every other call in flight.
- The daemon no longer cuts calls in flight when it is replaced by a new version or configuration: the successor takes the socket at once and serves new calls, while the calls the replaced daemon already took finish on it, for up to 30 minutes. A replacement started by an older harvester hands off the same way. `harvester stop` still drains and stops the daemon in place.
- An older installed harvester no longer replaces a newer running daemon: two plugin versions installed side by side (each launch re-points the installed link at its own build) replaced each other's daemon back and forth. `harvester setup` still restarts the daemon on the installed build.
- A proxy reaps each daemon it started once that daemon exits, where it left every one behind as a zombie process for as long as the proxy ran.
- A failed `ask` writes one `ask.failed` warning to the activity log with the engine, its time limit, how long it ran and the error, never the engine's output.

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
