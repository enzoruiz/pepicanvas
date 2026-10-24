/* global console, process */

// Runs the TE-010 remote render latency transmission on every browser of the
// support matrix against the latencyprobe harness, then validates the
// evidence with backend/cmd/latencycheck. Every Go invocation uses the Go
// toolchain pinned by the release manifest through GOTOOLCHAIN; the versions
// recorded in the evidence are the ones this run observes.

import { spawnSync } from "node:child_process";
import { mkdir, readFile, readdir, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  prepareBrowserCache,
  prepareExtractedBrowser,
} from "./browser-cache.mjs";
import { browserCachePaths, repositoryRoot } from "./browser-matrix.mjs";
import { buildVersions } from "./latency-evidence.mjs";

const frontendRoot = path.join(repositoryRoot, "frontend");
const backendRoot = path.join(repositoryRoot, "backend");
const manifestPath = path.join(
  repositoryRoot,
  "releases",
  "client-support-release.json",
);
const latencyRoot = path.join(frontendRoot, ".cache/latency");
const evidenceDir = path.join(latencyRoot, "evidence");
const versionsPath = path.join(latencyRoot, "versions.json");

function capture(command, args, options) {
  const result = spawnSync(command, args, { encoding: "utf8", ...options });
  if (result.error || result.status !== 0) {
    const detail = result.stderr?.trim() || result.error?.message || "failed";
    throw new Error(`${command} ${args.join(" ")}: ${detail}`);
  }
  return result.stdout;
}

async function observeVersions(goEnv) {
  const manifestText = await readFile(manifestPath, "utf8");
  return buildVersions({
    manifestText,
    goVersion: capture("go", ["env", "GOVERSION"], {
      cwd: backendRoot,
      env: goEnv,
    }),
    nodeVersion: process.versions.node,
    npmVersion: capture("npm", ["--version"], { cwd: frontendRoot }),
    goModText: await readFile(path.join(backendRoot, "go.mod"), "utf8"),
    lockText: await readFile(
      path.join(frontendRoot, "package-lock.json"),
      "utf8",
    ),
  });
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/latency-runner.mjs --download|--offline",
    );
  }
  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  const build = spawnSync(
    process.execPath,
    [
      path.join(frontendRoot, "node_modules/vite/bin/vite.js"),
      "build",
      "--config",
      "vite.latency.config.ts",
    ],
    { cwd: frontendRoot, stdio: "inherit" },
  );
  if (build.error || build.status !== 0) {
    throw new Error("latency probe build failed");
  }

  const manifest = JSON.parse(await readFile(manifestPath, "utf8"));
  const goEnv = {
    ...process.env,
    GOTOOLCHAIN: `go${manifest.toolchain.go.version}`,
  };
  await rm(evidenceDir, { force: true, recursive: true });
  await mkdir(evidenceDir, { recursive: true });
  await writeFile(
    versionsPath,
    `${JSON.stringify(await observeVersions(goEnv), null, 2)}\n`,
  );

  const failed = [];
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
          "playwright.latency.config.ts",
          `--project=${browser.slot}`,
        ],
        {
          cwd: frontendRoot,
          env: {
            ...goEnv,
            PEPICANVAS_LATENCY_EVIDENCE_DIR: evidenceDir,
            PEPICANVAS_LATENCY_VERSIONS: versionsPath,
          },
          stdio: "inherit",
        },
      );
      if (result.error || result.status !== 0) {
        failed.push(`${browser.slot} (playwright)`);
      }
    } finally {
      await rm(paths.extractionPath, { force: true, recursive: true });
    }
  }

  const produced = new Set(await readdir(evidenceDir));
  const evidenceFiles = [];
  for (const browser of matrix.browsers) {
    const name = `${browser.slot}.json`;
    if (produced.has(name)) {
      evidenceFiles.push(path.join(evidenceDir, name));
    } else {
      failed.push(`${browser.slot} (no evidence)`);
    }
  }

  if (evidenceFiles.length > 0) {
    const check = spawnSync(
      "go",
      [
        "run",
        "./cmd/latencycheck",
        "-manifest",
        "../releases/client-support-release.json",
        "-gomod",
        "go.mod",
        "-lock",
        "../frontend/package-lock.json",
        ...evidenceFiles,
      ],
      { cwd: backendRoot, env: goEnv, stdio: "inherit" },
    );
    if (check.error || check.status !== 0) {
      failed.push(`latencycheck (exit ${check.status ?? check.error.message})`);
    }
  }
  if (failed.length > 0) {
    throw new Error(`remote render latency failed: ${failed.join(", ")}`);
  }
  console.log(
    `remote render latency measured on ${matrix.browsers.length} browsers`,
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
