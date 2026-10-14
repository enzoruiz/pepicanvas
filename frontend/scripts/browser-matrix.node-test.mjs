import assert from "node:assert/strict";
import { mkdtemp, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  AUTHORIZED_BROWSER_SLOTS,
  downloadArchive,
  loadBrowserMatrix,
  verifyArchive,
} from "./browser-matrix.mjs";

const repositoryRoot = path.resolve(import.meta.dirname, "../..");
const matrixPath = path.join(
  repositoryRoot,
  "releases/client-browser-matrix.json",
);

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
