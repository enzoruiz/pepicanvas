/* global console, process */

import { spawnSync } from "node:child_process";
import { createHash, randomUUID } from "node:crypto";
import { mkdir, readFile, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  prepareBrowserCache,
  prepareExtractedBrowser,
} from "./browser-cache.mjs";
import { browserCachePaths, repositoryRoot } from "./browser-matrix.mjs";
import { validateBrowserEvidence } from "../../scripts/client-support/release-manifest.mjs";

function reportMetadata(reportName, reportBytes) {
  return {
    name: reportName,
    sha256: createHash("sha256").update(reportBytes).digest("hex"),
    size: reportBytes.length,
  };
}

function assertPassingReport(reportBytes) {
  let report;
  try {
    report = JSON.parse(reportBytes);
  } catch (error) {
    throw new Error(`Playwright report is not valid JSON: ${error.message}`, {
      cause: error,
    });
  }
  if (
    report === null ||
    typeof report !== "object" ||
    !Number.isSafeInteger(report.stats?.expected) ||
    report.stats.expected <= 0 ||
    report.stats.unexpected !== 0 ||
    report.stats.flaky !== 0
  ) {
    throw new Error("Playwright report does not prove a passing test run");
  }
}

function assertAuthorizedRuntime(runtimeIdentity) {
  if (
    runtimeIdentity?.os !== "linux" ||
    runtimeIdentity.architecture !== "x64"
  ) {
    throw new Error("browser evidence runtime must be Linux x64");
  }
}

export function buildBrowserEvidence({
  browser,
  startedAt,
  completedAt,
  correlationId,
  reportName,
  reportBytes,
  nodeVersion,
  playwrightVersion,
  runtimeIdentity,
  manifest,
}) {
  assertAuthorizedRuntime(runtimeIdentity);
  assertPassingReport(reportBytes);
  const evidence = {
    schemaVersion: 1,
    slot: browser.slot,
    browserVersion: browser.version,
    releaseDate: browser.releasedAt.slice(0, 10),
    scenarioId: browser.slot,
    result: "PASS",
    startedAt,
    completedAt,
    correlationId,
    runtime: {
      ...runtimeIdentity,
      node: nodeVersion,
      playwright: playwrightVersion,
    },
    outputs: [reportMetadata(reportName, reportBytes)],
  };
  if (manifest) validateBrowserEvidence(evidence, manifest);
  return evidence;
}

export async function emitBrowserEvidence({
  evidence,
  evidenceDirectory,
  manifest,
  reportBytes,
}) {
  validateBrowserEvidence(evidence, manifest);
  const expected = evidence.outputs[0];
  const actual = reportMetadata(expected.name, reportBytes);
  if (actual.size !== expected.size || actual.sha256 !== expected.sha256) {
    throw new Error("Playwright report bytes do not match browser evidence");
  }
  await mkdir(evidenceDirectory, { recursive: true });
  await writeFile(
    path.join(evidenceDirectory, `${evidence.slot}.json`),
    `${JSON.stringify(evidence, null, 2)}\n`,
    { flag: "wx" },
  );
}

export function buildPlaywrightInvocation({
  frontendRoot,
  browserSlot,
  reportPath,
  environment = process.env,
}) {
  const args = [
    path.join(frontendRoot, "node_modules", "@playwright", "test", "cli.js"),
    "test",
    "--project",
    browserSlot,
  ];
  const env = { ...environment };
  if (reportPath) {
    args.push("--reporter", "json");
    env.PLAYWRIGHT_JSON_OUTPUT_NAME = reportPath;
  }
  return {
    args,
    options: { cwd: frontendRoot, env, stdio: "inherit" },
  };
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  const evidenceDirectory =
    rest.length === 2 && rest[0] === "--evidence-dir"
      ? path.resolve(rest[1])
      : undefined;
  if (
    (!evidenceDirectory && rest.length > 0) ||
    !["--download", "--offline"].includes(mode)
  ) {
    throw new Error(
      "usage: node scripts/browser-matrix-runner.mjs --download|--offline [--evidence-dir <directory>]",
    );
  }

  const frontendRoot = path.join(repositoryRoot, "frontend");
  const manifest = evidenceDirectory
    ? JSON.parse(
        await readFile(
          path.join(
            repositoryRoot,
            "releases/client-support-release.template.json",
          ),
          "utf8",
        ),
      )
    : undefined;
  const playwrightPackage = evidenceDirectory
    ? JSON.parse(
        await readFile(
          path.join(frontendRoot, "node_modules/@playwright/test/package.json"),
          "utf8",
        ),
      )
    : undefined;
  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  const extractedRoot = path.join(
    frontendRoot,
    ".cache",
    "client-browsers",
    "extracted",
  );
  const reportRoot = path.join(
    frontendRoot,
    "test-results",
    "browser-evidence",
  );
  await rm(extractedRoot, { force: true, recursive: true });
  if (evidenceDirectory) {
    await rm(reportRoot, { force: true, recursive: true });
    await mkdir(reportRoot, { recursive: true });
  }

  for (const browser of matrix.browsers) {
    const paths = browserCachePaths(frontendRoot, browser);
    try {
      const { versionOutput } = await prepareExtractedBrowser(browser);
      console.log(`${browser.slot}: executing ${versionOutput}`);
      const startedAt = evidenceDirectory
        ? new Date().toISOString()
        : undefined;
      const correlationId = evidenceDirectory
        ? `${browser.slot}-${randomUUID()}`
        : undefined;
      const reportName = correlationId
        ? `${correlationId}-report.json`
        : undefined;
      const reportPath = reportName
        ? path.join(reportRoot, reportName)
        : undefined;
      const invocation = buildPlaywrightInvocation({
        frontendRoot,
        browserSlot: browser.slot,
        reportPath,
      });
      const result = spawnSync(
        process.execPath,
        invocation.args,
        invocation.options,
      );
      if (result.error || result.status !== 0) {
        throw new Error(
          `${browser.slot} Playwright scenario failed${
            result.error ? `: ${result.error.message}` : ""
          }`,
        );
      }
      if (evidenceDirectory) {
        const completedAt = new Date().toISOString();
        const reportBytes = await readFile(reportPath);
        const evidence = buildBrowserEvidence({
          browser,
          startedAt,
          completedAt,
          correlationId,
          reportName,
          reportBytes,
          nodeVersion: process.versions.node,
          playwrightVersion: playwrightPackage.version,
          runtimeIdentity: {
            os: process.platform,
            architecture: process.arch,
          },
          manifest,
        });
        await emitBrowserEvidence({
          evidence,
          evidenceDirectory,
          manifest,
          reportBytes,
        });
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
