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

const projects = matrixBrowsers.map((browser) => ({
  name: browser.slot,
  use: {
    ...(browser.family === "edge"
      ? devices["Desktop Edge"]
      : devices["Desktop Chrome"]),
    launchOptions: {
      executablePath: browser.executablePath,
    },
  },
}));

export default defineConfig({
  testDir: "./tests/browser",
  testMatch: "**/*.playwright.mjs",
  fullyParallel: false,
  projects,
  reporter: "line",
  retries: 0,
  workers: 1,
  use: {
    baseURL: "http://127.0.0.1:4173",
    headless: true,
    trace: "off",
  },
  webServer: {
    command: "npm run dev -- --host 127.0.0.1 --port 4173",
    reuseExistingServer: false,
    url: "http://127.0.0.1:4173/editor/",
  },
});
