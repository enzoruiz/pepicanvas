/* global console, process */

import { spawnSync } from "node:child_process";
import { access, mkdir, rename, rm, stat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  browserCachePaths,
  downloadArchive,
  loadBrowserMatrix,
  repositoryRoot,
  verifyArchive,
} from "./browser-matrix.mjs";

function requireCommand(command) {
  const versionArguments = command === "unzip" ? ["-v"] : ["--version"];
  const result = spawnSync(command, versionArguments, { encoding: "utf8" });
  if (result.error || result.status !== 0) {
    throw new Error(`${command} is required but unavailable`);
  }
}

function run(command, argumentsList) {
  const result = spawnSync(command, argumentsList, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error || result.status !== 0) {
    const detail = result.stderr?.trim() || result.error?.message || "failed";
    throw new Error(`${command} ${argumentsList.join(" ")}: ${detail}`);
  }
  return result.stdout.trim();
}

async function extractBrowser(browser, archivePath, extractionPath) {
  const temporaryPath = `${extractionPath}.partial-${process.pid}`;
  await rm(temporaryPath, { force: true, recursive: true });
  await mkdir(path.dirname(extractionPath), { recursive: true });
  await mkdir(temporaryPath, { recursive: true });
  try {
    if (browser.family === "chrome") {
      run("unzip", ["-q", archivePath, "-d", temporaryPath]);
    } else {
      run("dpkg-deb", ["--extract", archivePath, temporaryPath]);
    }
    await rm(extractionPath, { force: true, recursive: true });
    await rename(temporaryPath, extractionPath);
  } finally {
    await rm(temporaryPath, { force: true, recursive: true });
  }
}

async function verifyExecutable(browser, executablePath, extractionPath) {
  const relative = path.relative(extractionPath, executablePath);
  if (relative.startsWith("..") || path.isAbsolute(relative)) {
    throw new Error(`${browser.slot} executable escapes its extraction path`);
  }
  const executableStat = await stat(executablePath);
  if (!executableStat.isFile()) {
    throw new Error(`${browser.slot} executable is not a regular file`);
  }
  await access(executablePath);
  const versionOutput = run(executablePath, ["--version"]);
  if (!versionOutput.includes(browser.version)) {
    throw new Error(
      `${browser.slot} executable reported unexpected version: ${versionOutput}`,
    );
  }
  return versionOutput;
}

export async function prepareBrowserCache({ offline }) {
  requireCommand("unzip");
  requireCommand("dpkg-deb");

  const matrixPath = path.join(
    repositoryRoot,
    "releases",
    "client-browser-matrix.json",
  );
  const frontendRoot = path.join(repositoryRoot, "frontend");
  const matrix = await loadBrowserMatrix(matrixPath);

  for (const browser of matrix.browsers) {
    const paths = browserCachePaths(frontendRoot, browser);
    if (!offline) {
      await downloadArchive(browser, paths.archivePath);
    }
    try {
      await access(paths.archivePath);
    } catch {
      throw new Error(
        `${browser.slot} archive is absent; offline mode cannot download it`,
      );
    }
    const verified = await verifyArchive(paths.archivePath, browser);
    console.log(
      `${browser.slot}: ${verified.size} bytes; sha256:${verified.sha256}`,
    );
  }

  return matrix;
}

export async function prepareExtractedBrowser(browser) {
  const frontendRoot = path.join(repositoryRoot, "frontend");
  const paths = browserCachePaths(frontendRoot, browser);
  await verifyArchive(paths.archivePath, browser);
  await extractBrowser(browser, paths.archivePath, paths.extractionPath);
  const versionOutput = await verifyExecutable(
    browser,
    paths.executablePath,
    paths.extractionPath,
  );
  return { ...paths, versionOutput };
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/browser-cache.mjs --download|--offline",
    );
  }
  await prepareBrowserCache({ offline: mode === "--offline" });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
