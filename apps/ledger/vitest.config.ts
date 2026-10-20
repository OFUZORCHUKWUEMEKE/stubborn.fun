import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

// Tests run inside workerd, the real Workers runtime, against real
// SQLite-backed Durable Object storage — not mocks.
export default defineConfig({
  plugins: [cloudflareTest({ wrangler: { configPath: "./wrangler.jsonc" } })],
  test: {
    testTimeout: 60_000,
  },
});
