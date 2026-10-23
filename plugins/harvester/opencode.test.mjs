import assert from "node:assert/strict";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { HarvesterPlugin } from "./opencode.js";

test("registers the shared launcher without replacing other MCP servers", async () => {
  const other = { type: "local", command: ["other-server"] };
  const config = { mcp: { other }, theme: "system" };
  const hook = await HarvesterPlugin();
  await hook.config(config);
  assert.deepEqual(config, {
    mcp: {
      other,
      harvester: {
        type: "local",
        command: [fileURLToPath(new URL("./libexec/harvester", import.meta.url)), "mcp"],
        enabled: true,
      },
    },
    theme: "system",
  });
});

test("creates the MCP map and keeps an explicit operator registration", async () => {
  const hook = await HarvesterPlugin();
  const empty = {};
  await hook.config(empty);
  assert.equal(empty.mcp.harvester.enabled, true);
  const custom = { type: "local", command: ["custom", "mcp"], enabled: false };
  const config = { mcp: { harvester: custom } };
  await hook.config(config);
  assert.equal(config.mcp.harvester, custom);
});
