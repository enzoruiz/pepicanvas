import { execFileSync } from "node:child_process";
import { resolve } from "node:path";

import { defineConfig, devices } from "@playwright/test";

interface MatrixBrowser {
  executablePath: string;
  family: "chrome" | "edge";
  slot: string;
}

const matrixBrowsers = JSON.parse(
  execFileSync(
    process.execPath,
    [resolve(import.meta.dirname, "scripts/browser-projects.mjs")],
    { encoding: "utf8" },
  ),
) as MatrixBrowser[];

// The Go server serves the production build and the API from one origin, as
// in production. localhost is a secure context, so the __Host- cookie works
// over http. The runner builds the frontend first.
export const editorOrigin = "http://localhost:4176";
const listenAddress = "127.0.0.1:4176";

const databaseURL = process.env.PEPICANVAS_E2E_DATABASE_URL;
if (!databaseURL) {
  throw new Error(
    "PEPICANVAS_E2E_DATABASE_URL is required; run npm run auth:e2e",
  );
}

export default defineConfig({
  testDir: "./tests/browser",
  testMatch: "**/*.auth.mjs",
  outputDir: resolve(import.meta.dirname, ".cache/auth-e2e/test-results"),
  fullyParallel: false,
  projects: matrixBrowsers.map((browser) => ({
    name: browser.slot,
    use: {
      ...(browser.family === "edge"
        ? devices["Desktop Edge"]
        : devices["Desktop Chrome"]),
      launchOptions: {
        executablePath: browser.executablePath,
        // The cache scenario needs the browser's real back/forward cache.
        ignoreDefaultArgs: ["--disable-back-forward-cache"],
      },
    },
  })),
  expect: { timeout: 10_000 },
  reporter: "line",
  retries: 0,
  workers: 1,
  use: {
    baseURL: editorOrigin,
    headless: true,
    trace: "off",
  },
  webServer: {
    command: "go run ./cmd/server",
    cwd: resolve(import.meta.dirname, "../backend"),
    env: {
      PEPICANVAS_DATABASE_URL: databaseURL,
      PEPICANVAS_ALLOWED_ORIGIN: editorOrigin,
      PEPICANVAS_LISTEN_ADDR: listenAddress,
      PEPICANVAS_STATIC_DIR: resolve(import.meta.dirname, "dist"),
    },
    reuseExistingServer: false,
    url: `http://${listenAddress}/api/session`,
  },
});
