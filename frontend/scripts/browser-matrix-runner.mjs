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

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/browser-matrix-runner.mjs --download|--offline",
    );
  }

  const frontendRoot = path.join(repositoryRoot, "frontend");
  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  const extractedRoot = path.join(
    frontendRoot,
    ".cache",
    "client-browsers",
    "extracted",
  );
  await rm(extractedRoot, { force: true, recursive: true });

  for (const browser of matrix.browsers) {
    const paths = browserCachePaths(frontendRoot, browser);
    try {
      const { versionOutput } = await prepareExtractedBrowser(browser);
      console.log(`${browser.slot}: executing ${versionOutput}`);
      const result = spawnSync(
        process.execPath,
        [
          path.join(
            frontendRoot,
            "node_modules",
            "@playwright",
            "test",
            "cli.js",
          ),
          "test",
          "--project",
          browser.slot,
        ],
        { cwd: frontendRoot, stdio: "inherit" },
      );
      if (result.error || result.status !== 0) {
        throw new Error(
          `${browser.slot} Playwright scenario failed${
            result.error ? `: ${result.error.message}` : ""
          }`,
        );
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
