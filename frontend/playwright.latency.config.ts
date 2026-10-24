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

// latencyprobe serves the probe page and the harness API from one loopback
// origin. The runner builds the probe page and pins the Go toolchain first.
const listenAddress = "127.0.0.1:4177";

const goToolchain = process.env.GOTOOLCHAIN;
if (!goToolchain) {
  throw new Error("GOTOOLCHAIN is required; run npm run latency:probe");
}

export default defineConfig({
  testDir: "./tests/browser",
  testMatch: "**/*.latency.mjs",
  outputDir: resolve(import.meta.dirname, ".cache/latency/test-results"),
  fullyParallel: false,
  projects: matrixBrowsers.map((browser) => ({
    name: browser.slot,
    use: {
      ...(browser.family === "edge"
        ? devices["Desktop Edge"]
        : devices["Desktop Chrome"]),
      launchOptions: { executablePath: browser.executablePath },
    },
  })),
  reporter: "line",
  retries: 0,
  // 120 samples with seven round trips and a render each.
  timeout: 300_000,
  workers: 1,
  use: {
    baseURL: `http://${listenAddress}`,
    headless: true,
    trace: "off",
  },
  webServer: {
    command: `go run ./cmd/latencyprobe -listen ${listenAddress} -static ${resolve(import.meta.dirname, ".cache/latency/dist")}`,
    cwd: resolve(import.meta.dirname, "../backend"),
    env: { GOTOOLCHAIN: goToolchain },
    reuseExistingServer: false,
    timeout: 180_000,
    url: `http://${listenAddress}/probe/ping`,
  },
});
