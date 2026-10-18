/* global console, process */

import { spawnSync } from "node:child_process";
import { cp, mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  prepareBrowserCache,
  prepareExtractedBrowser,
} from "./browser-cache.mjs";
import { browserCachePaths, repositoryRoot } from "./browser-matrix.mjs";
import { evaluateSlot } from "./privacy-evidence.mjs";

const frontendRoot = path.join(repositoryRoot, "frontend");
const privacyCachePath = path.join(frontendRoot, ".cache/privacy-fixture");
const fixtureLogPath = path.join(privacyCachePath, "fixture.log");
const decoysPath = path.join(privacyCachePath, "decoys.json");
const testResultsPath = path.join(privacyCachePath, "test-results");
// Records the fixture writes outside any request; every other record must
// carry a correlation ID.
const processMessages = ["privacy fixture listening"];

async function resetRunOutputs(slotPath) {
  for (const target of [
    slotPath,
    fixtureLogPath,
    decoysPath,
    testResultsPath,
  ]) {
    await rm(target, { force: true, recursive: true });
  }
}

// collectEvidence copies this slot's surfaces out of the shared cache so the
// next slot cannot overwrite them, keeping the decoys beside, not inside, the
// scanned evidence.
async function collectEvidence(slotPath, output) {
  const evidenceRoot = path.join(slotPath, "evidence");
  await mkdir(evidenceRoot, { recursive: true });
  await writeFile(path.join(evidenceRoot, "playwright-output.txt"), output);
  await cp(fixtureLogPath, path.join(evidenceRoot, "fixture.log")).catch(
    (error) => {
      if (error.code !== "ENOENT") throw error;
    },
  );
  await cp(testResultsPath, path.join(evidenceRoot, "test-results"), {
    recursive: true,
  }).catch((error) => {
    if (error.code !== "ENOENT") throw error;
  });
  let decoys;
  try {
    decoys = JSON.parse(await readFile(decoysPath, "utf8"));
  } catch (error) {
    throw new Error(`privacy run did not record decoys: ${error.message}`, {
      cause: error,
    });
  }
  await cp(decoysPath, path.join(slotPath, "decoys.json"));
  return { decoys, evidenceRoot };
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/privacy-runner.mjs --download|--offline",
    );
  }

  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  const earlier = [];
  for (const browser of matrix.browsers) {
    const paths = browserCachePaths(frontendRoot, browser);
    const slotPath = path.join(privacyCachePath, "slots", browser.slot);
    try {
      await resetRunOutputs(slotPath);
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
        {
          cwd: frontendRoot,
          encoding: "utf8",
          maxBuffer: 64 * 1024 * 1024,
          stdio: ["inherit", "pipe", "pipe"],
        },
      );
      process.stdout.write(result.stdout ?? "");
      process.stderr.write(result.stderr ?? "");
      const scenarioFailed = Boolean(result.error) || result.status !== 0;

      const { decoys, evidenceRoot } = await collectEvidence(
        slotPath,
        `${result.stdout ?? ""}${result.stderr ?? ""}`,
      );
      const findings = await evaluateSlot({
        slot: browser.slot,
        decoys,
        earlier,
        evidenceRoot,
        processMessages,
      });
      for (const finding of findings) {
        console.error(`${browser.slot}: ${finding.rule}: ${finding.detail}`);
      }
      if (scenarioFailed) {
        throw new Error(`${browser.slot} privacy scenario failed`);
      }
      if (findings.length > 0) {
        throw new Error(
          `${browser.slot} privacy evidence has ${findings.length} finding(s)`,
        );
      }
      console.log(
        `${browser.slot}: evidence clean against ${earlier.length + 1} decoy set(s)`,
      );
      earlier.push({ slot: browser.slot, decoys });
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
