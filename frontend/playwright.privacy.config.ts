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

export const fixtureOrigin = "http://127.0.0.1:4174";
// The privacy runner collects everything under this directory as evidence
// after each slot: the fixture log, Playwright output, and recorded decoys.
export const privacyCachePath = resolve(
  import.meta.dirname,
  ".cache/privacy-fixture",
);
export const fixtureLogPath = resolve(privacyCachePath, "fixture.log");

export default defineConfig({
  testDir: "./tests/browser",
  testMatch: "**/*.privacy.mjs",
  outputDir: resolve(privacyCachePath, "test-results"),
  fullyParallel: false,
  projects: matrixBrowsers.map((browser) => ({
    name: browser.slot,
    use: {
      ...(browser.family === "edge"
        ? devices["Desktop Edge"]
        : devices["Desktop Chrome"]),
      launchOptions: {
        executablePath: browser.executablePath,
        // Playwright disables the back/forward cache by default; the access
        // cut test must run against the browser's real restore behavior.
        ignoreDefaultArgs: ["--disable-back-forward-cache"],
      },
    },
  })),
  reporter: "line",
  retries: 0,
  workers: 1,
  use: {
    baseURL: "http://127.0.0.1:4173",
    headless: true,
    trace: "off",
  },
  webServer: [
    {
      command: "npm run dev -- --host 127.0.0.1 --port 4173",
      reuseExistingServer: false,
      url: "http://127.0.0.1:4173/editor/",
    },
    {
      command: `go run ./test/privacyfixture -addr 127.0.0.1:4174 -log ${fixtureLogPath}`,
      cwd: resolve(import.meta.dirname, "../backend"),
      reuseExistingServer: false,
      url: `${fixtureOrigin}/__fixture/decoys`,
    },
  ],
});
