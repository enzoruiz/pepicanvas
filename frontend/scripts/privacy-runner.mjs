/* global console, process */

import { spawnSync } from "node:child_process";
import { rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  prepareBrowserCache,
  prepareExtractedBrowser,
} from "./browser-cache.mjs";
import { browserCachePaths, repositoryRoot } from "./browser-matrix.mjs";

const frontendRoot = path.join(repositoryRoot, "frontend");

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/privacy-runner.mjs --download|--offline",
    );
  }

  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  for (const browser of matrix.browsers) {
    const paths = browserCachePaths(frontendRoot, browser);
    try {
      const { versionOutput } = await prepareExtractedBrowser(browser);
      console.log(`${browser.slot}: executing ${versionOutput}`);
      const result = spawnSync(
        process.execPath,
        [
          path.join(frontendRoot, "node_modules/@playwright/test/cli.js"),
          "test",
          "--config",
          "playwright.privacy.config.ts",
          "--project",
          browser.slot,
        ],
        { cwd: frontendRoot, stdio: "inherit" },
      );
      if (result.error || result.status !== 0) {
        throw new Error(`${browser.slot} privacy scenario failed`);
      }
    } finally {
      await rm(paths.extractionPath, { force: true, recursive: true });
    }
  }
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
