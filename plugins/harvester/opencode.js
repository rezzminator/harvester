import { fileURLToPath } from "node:url";

export const HarvesterPlugin = async () => ({
  config: async (config) => {
    config.mcp ??= {};
    config.mcp.harvester ??= {
      type: "local",
      command: [fileURLToPath(new URL("./libexec/harvester", import.meta.url)), "mcp"],
      enabled: true,
    };
  },
});
