---
description: 'Checks install and connection health — asks like "is harvester working", "why is search_web missing", "reads fail". Returns each health row, every warning with its fix, and what went unchecked.'
---

# Harvester health

Report only; the user decides on fixes.

1. Run with Bash: `"${CLAUDE_PLUGIN_ROOT}/libexec/harvester" doctor`. Each output line is a row, `doctor: {area} key=value…`. Exit 0: no warnings. Exit 1 with rows: at least one warning. A row's fix is its `hint=` text or `remove=` command. Any other exit, or no rows: the doctor itself failed; quote its output and go on.
2. Call `mcp__plugin_harvester_harvester__read` once with `{"urls": ["https://example.com"], "refresh": true}`, so the page is fetched rather than served from cache. Healthy: one `=== [1/1]` block holding the page's text. The tool absent from this chat: the harvester's MCP server is not connected here; reconnect it with `/mcp` or start a new chat. An `error:` answer: quote it.
3. When this chat lists `mcp__plugin_harvester_harvester__search_web`, call it once with `{"query": "harvester"}`: one result or more is healthy, `No results found` is a working backend that matched nothing, an error answer is a failed backend; quote it. When it is not listed, give the reason the `doctor: tools` row names; a chat keeps the tool list it started with, so once that row says `search_web=listed`, a new chat has the tool.
4. Report in plain words, grouped by area: each row's state; every row carrying `error=`, `hint=` or `remove=`, with its fix; the read's and the search's outcome; then, under "Not checked", each step that could not run and why.
