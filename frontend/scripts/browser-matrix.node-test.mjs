import assert from "node:assert/strict";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  AUTHORIZED_BROWSER_SLOTS,
  downloadArchive,
  loadBrowserMatrix,
  verifyArchive,
} from "./browser-matrix.mjs";
import {
  buildBrowserEvidence,
  buildPlaywrightInvocation,
  emitBrowserEvidence,
} from "./browser-matrix-runner.mjs";

const repositoryRoot = path.resolve(import.meta.dirname, "../..");
const matrixPath = path.join(
  repositoryRoot,
  "releases/client-browser-matrix.json",
);
const evidenceManifest = {
  clients: [
    {
      family: "chrome",
      slot: "current",
      version: "155.0.8059.79",
      releaseDate: "2026-10-13",
    },
  ],
  scenarios: [
    {
      id: "chrome-current",
      clientFamily: "chrome",
      clientSlot: "current",
    },
  ],
};

test("ordinary browser runs preserve Playwright reporter behavior", () => {
  const environment = {
    PLAYWRIGHT_JSON_OUTPUT_NAME: "caller-selected-report.json",
    PLAYWRIGHT_HTML_OPEN: "never",
  };

  const invocation = buildPlaywrightInvocation({
    frontendRoot: "/workspace/frontend",
    browserSlot: "chrome-current",
    environment,
  });

  assert.deepEqual(invocation.args, [
    "/workspace/frontend/node_modules/@playwright/test/cli.js",
    "test",
    "--project",
    "chrome-current",
  ]);
  assert.deepEqual(invocation.options.env, environment);
  assert.equal(invocation.options.stdio, "inherit");
});

test("evidence browser runs select the JSON reporter and output file", () => {
  const invocation = buildPlaywrightInvocation({
    frontendRoot: "/workspace/frontend",
    browserSlot: "chrome-current",
    reportPath: "/workspace/report.json",
    environment: { PLAYWRIGHT_HTML_OPEN: "never" },
  });

  assert.deepEqual(invocation.args.slice(-2), ["--reporter", "json"]);
  assert.equal(
    invocation.options.env.PLAYWRIGHT_JSON_OUTPUT_NAME,
    "/workspace/report.json",
  );
});

test("the checked-in matrix contains only the four authorized exact releases", async () => {
  const matrix = await loadBrowserMatrix(matrixPath);

  assert.deepEqual(
    matrix.browsers.map(({ slot, version }) => ({ slot, version })),
    AUTHORIZED_BROWSER_SLOTS.map(({ slot, version }) => ({ slot, version })),
  );
  assert.equal(matrix.platform, "linux");
  assert.equal(matrix.architecture, "x64");
});

test("strict matrix decoding rejects unknown fields and release drift", async () => {
  const source = JSON.parse(
    await (await import("node:fs/promises")).readFile(matrixPath, "utf8"),
  );
  const directory = await mkdtemp(path.join(os.tmpdir(), "pepicanvas-matrix-"));
  const unknownFieldPath = path.join(directory, "unknown-field.json");
  const driftPath = path.join(directory, "drift.json");

  await writeFile(
    unknownFieldPath,
    JSON.stringify({ ...source, unexpected: true }),
  );
  source.browsers[0].version = "155.0.8059.80";
  await writeFile(driftPath, JSON.stringify(source));

  await assert.rejects(loadBrowserMatrix(unknownFieldPath), /unknown field/i);
  await assert.rejects(loadBrowserMatrix(driftPath), /authorized version/i);
});

test("archive verification rejects size and SHA-256 mismatches", async () => {
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-archive-"),
  );
  const archivePath = path.join(directory, "archive.zip");
  const contents = "not an authorized browser archive";
  await writeFile(archivePath, contents);

  await assert.rejects(
    verifyArchive(archivePath, {
      archiveSha256: "0".repeat(64),
      archiveSize: contents.length,
    }),
    /SHA-256/i,
  );
  await assert.rejects(
    verifyArchive(archivePath, {
      archiveSha256: "0".repeat(64),
      archiveSize: 1,
    }),
    /size/i,
  );
});

test("the downloader rejects URL drift before network access", async () => {
  const browser = {
    ...AUTHORIZED_BROWSER_SLOTS[0],
    archiveUrl:
      "https://storage.googleapis.com/chrome-for-testing-public/155.0.8059.80/linux64/chrome-linux64.zip",
  };
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-download-"),
  );

  await assert.rejects(
    downloadArchive(browser, path.join(directory, "archive.zip")),
    /not authorized/i,
  );
});

test("browser evidence binds exact report bytes and runtime identity", () => {
  const reportBytes = Buffer.from(
    JSON.stringify({ stats: { expected: 1, unexpected: 0, flaky: 0 } }),
  );
  const evidence = buildBrowserEvidence({
    browser: AUTHORIZED_BROWSER_SLOTS[0],
    startedAt: "2026-10-17T20:00:00.000Z",
    completedAt: "2026-10-17T20:00:01.000Z",
    correlationId: "chrome-current-00000000-0000-4000-8000-000000000001",
    reportName: "chrome-current-report.json",
    reportBytes,
    nodeVersion: "24.16.0",
    playwrightVersion: "1.64.0",
    runtimeIdentity: { os: "linux", architecture: "x64" },
    manifest: evidenceManifest,
  });

  assert.deepEqual(evidence, {
    schemaVersion: 1,
    slot: "chrome-current",
    browserVersion: "155.0.8059.79",
    releaseDate: "2026-10-13",
    scenarioId: "chrome-current",
    result: "PASS",
    startedAt: "2026-10-17T20:00:00.000Z",
    completedAt: "2026-10-17T20:00:01.000Z",
    correlationId: "chrome-current-00000000-0000-4000-8000-000000000001",
    runtime: {
      os: "linux",
      architecture: "x64",
      node: "24.16.0",
      playwright: "1.64.0",
    },
    outputs: [
      {
        name: "chrome-current-report.json",
        sha256:
          "b165c5bef91f093a36bdd36a5badb84ca8b38cee697fe13a27d87ec2924f91a6",
        size: reportBytes.length,
      },
    ],
  });
});

test("browser evidence rejects unsupported runtime identities", () => {
  const options = {
    browser: AUTHORIZED_BROWSER_SLOTS[0],
    startedAt: "2026-10-17T20:00:00.000Z",
    completedAt: "2026-10-17T20:00:01.000Z",
    correlationId: "chrome-current-00000000-0000-4000-8000-000000000001",
    reportName: "chrome-current-report.json",
    reportBytes: Buffer.from(
      JSON.stringify({ stats: { expected: 1, unexpected: 0, flaky: 0 } }),
    ),
    nodeVersion: "24.16.0",
    playwrightVersion: "1.64.0",
    manifest: evidenceManifest,
  };

  for (const runtimeIdentity of [
    { os: "win32", architecture: "x64" },
    { os: "linux", architecture: "arm64" },
  ]) {
    assert.throws(
      () => buildBrowserEvidence({ ...options, runtimeIdentity }),
      /runtime.*Linux x64/i,
    );
  }
});

test("browser evidence emission fails closed for incomplete or changed reports", async () => {
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-browser-evidence-"),
  );
  const reportBytes = Buffer.from(
    JSON.stringify({ stats: { expected: 1, unexpected: 0, flaky: 0 } }),
  );
  const options = {
    browser: AUTHORIZED_BROWSER_SLOTS[0],
    startedAt: "2026-10-17T20:00:00.000Z",
    completedAt: "2026-10-17T20:00:01.000Z",
    correlationId: "chrome-current-00000000-0000-4000-8000-000000000001",
    reportName: "chrome-current-report.json",
    reportBytes,
    nodeVersion: "24.16.0",
    playwrightVersion: "1.64.0",
    runtimeIdentity: { os: "linux", architecture: "x64" },
    manifest: evidenceManifest,
  };
  const evidence = buildBrowserEvidence(options);

  await assert.rejects(
    emitBrowserEvidence({
      evidence,
      evidenceDirectory: directory,
      manifest: evidenceManifest,
      reportBytes: Buffer.from("changed"),
    }),
    /report.*match/i,
  );
  assert.throws(
    () =>
      buildBrowserEvidence({
        ...options,
        reportBytes: Buffer.from(
          JSON.stringify({ stats: { expected: 0, unexpected: 1, flaky: 0 } }),
        ),
      }),
    /passing test/i,
  );
  await assert.rejects(readFile(path.join(directory, "chrome-current.json")));

  await emitBrowserEvidence({
    evidence,
    evidenceDirectory: directory,
    manifest: evidenceManifest,
    reportBytes,
  });
  assert.deepEqual(
    JSON.parse(await readFile(path.join(directory, "chrome-current.json"))),
    evidence,
  );
});
