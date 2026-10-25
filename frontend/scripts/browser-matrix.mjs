import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { access, mkdir, readFile, rename, rm, stat } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

const chromeSources = [
  "https://googlechromelabs.github.io/chrome-for-testing/last-known-good-versions-with-downloads.json",
  "https://googlechromelabs.github.io/chrome-for-testing/latest-versions-per-milestone-with-downloads.json",
];
const edgeSources = [
  "https://edgeupdates.microsoft.com/api/products?view=enterprise",
];

export const AUTHORIZED_BROWSER_SLOTS = Object.freeze([
  {
    slot: "chrome-current",
    family: "chrome",
    releaseTrack: "current",
    version: "155.0.8059.79",
    releasedAt: "2026-10-13T20:44:51.420550Z",
    platform: "linux",
    architecture: "x64",
    metadataSourceUrls: chromeSources,
    archiveUrl:
      "https://storage.googleapis.com/chrome-for-testing-public/155.0.8059.79/linux64/chrome-linux64.zip",
    archiveSize: 198413482,
    archiveSizeProvenance: "observed-bootstrap",
    archiveSha256:
      "bcd9a79130d7e44d70f93be87042e9bcee7fe0f0b3898e06b8c9bed5fb619dcd",
    hashProvenance: "observed-bootstrap",
    executableRelativePath: "chrome-linux64/chrome",
  },
  {
    slot: "chrome-previous",
    family: "chrome",
    releaseTrack: "previous",
    version: "154.0.8037.92",
    releasedAt: "2026-09-29T18:30:29.983968Z",
    platform: "linux",
    architecture: "x64",
    metadataSourceUrls: chromeSources,
    archiveUrl:
      "https://storage.googleapis.com/chrome-for-testing-public/154.0.8037.92/linux64/chrome-linux64.zip",
    archiveSize: 196202491,
    archiveSizeProvenance: "observed-bootstrap",
    archiveSha256:
      "ff43322f335e436b2f4dcdfeeec5db032299e335a7e8c1c618b326e100ce8732",
    hashProvenance: "observed-bootstrap",
    executableRelativePath: "chrome-linux64/chrome",
  },
  {
    slot: "edge-current",
    family: "edge",
    releaseTrack: "current",
    version: "155.0.4283.45",
    releasedAt: "2026-10-08T18:26:00",
    platform: "linux",
    architecture: "x64",
    metadataSourceUrls: edgeSources,
    archiveUrl:
      "https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/microsoft-edge-stable_155.0.4283.45-1_amd64.deb",
    archiveSize: 194372662,
    archiveSizeProvenance: "official",
    archiveSha256:
      "35ac88189ed91b60cff446fb6020ca60513ce20f985010d3c5b996115e3ec4a4",
    hashProvenance: "official",
    executableRelativePath: "opt/microsoft/msedge/msedge",
  },
  {
    slot: "edge-previous",
    family: "edge",
    releaseTrack: "previous",
    version: "154.0.4258.62",
    releasedAt: "2026-10-05T16:45:00",
    platform: "linux",
    architecture: "x64",
    metadataSourceUrls: edgeSources,
    archiveUrl:
      "https://packages.microsoft.com/repos/edge/pool/main/m/microsoft-edge-stable/microsoft-edge-stable_154.0.4258.62-1_amd64.deb",
    archiveSize: 192845122,
    archiveSizeProvenance: "official",
    archiveSha256:
      "f425dea59f0a8bac00201685a070e947543c57d135c130ba73e0863cf2c506b1",
    hashProvenance: "official",
    executableRelativePath: "opt/microsoft/msedge/msedge",
  },
]);

const rootFields = ["architecture", "browsers", "platform", "schemaVersion"];
const browserFields = [
  "architecture",
  "archiveSha256",
  "archiveSize",
  "archiveSizeProvenance",
  "archiveUrl",
  "executableRelativePath",
  "family",
  "hashProvenance",
  "metadataSourceUrls",
  "platform",
  "releasedAt",
  "releaseTrack",
  "slot",
  "version",
];

function assertExactFields(value, expectedFields, location) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${location} must be an object`);
  }
  const actualFields = Object.keys(value).sort();
  const expected = [...expectedFields].sort();
  const unknown = actualFields.filter((field) => !expected.includes(field));
  if (unknown.length > 0) {
    throw new Error(`${location} contains unknown field ${unknown[0]}`);
  }
  const missing = expected.filter((field) => !actualFields.includes(field));
  if (missing.length > 0) {
    throw new Error(`${location} is missing field ${missing[0]}`);
  }
}

function assertAuthorizedBrowser(actual, expected, index) {
  assertExactFields(actual, browserFields, `browsers[${index}]`);
  for (const field of browserFields) {
    const actualValue = actual[field];
    const expectedValue = expected[field];
    if (JSON.stringify(actualValue) !== JSON.stringify(expectedValue)) {
      const description = field === "version" ? "authorized version" : field;
      throw new Error(
        `${actual.slot ?? `browsers[${index}]`} does not match ${description}`,
      );
    }
  }

  const archiveUrl = new URL(actual.archiveUrl);
  if (archiveUrl.protocol !== "https:") {
    throw new Error(`${actual.slot} archive URL must use HTTPS`);
  }
}

export async function loadBrowserMatrix(matrixPath) {
  const source = await readFile(matrixPath, "utf8");
  let matrix;
  try {
    matrix = JSON.parse(source);
  } catch (error) {
    throw new Error(`browser matrix is not valid JSON: ${error.message}`, {
      cause: error,
    });
  }

  assertExactFields(matrix, rootFields, "browser matrix");
  if (
    matrix.schemaVersion !== 1 ||
    matrix.platform !== "linux" ||
    matrix.architecture !== "x64" ||
    !Array.isArray(matrix.browsers) ||
    matrix.browsers.length !== AUTHORIZED_BROWSER_SLOTS.length
  ) {
    throw new Error(
      "browser matrix root contract does not match authorization",
    );
  }

  matrix.browsers.forEach((browser, index) => {
    assertAuthorizedBrowser(browser, AUTHORIZED_BROWSER_SLOTS[index], index);
  });
  const slots = new Set(matrix.browsers.map(({ slot }) => slot));
  if (slots.size !== AUTHORIZED_BROWSER_SLOTS.length) {
    throw new Error("browser matrix contains duplicate slots");
  }

  return matrix;
}

export async function verifyArchive(archivePath, browser) {
  const archiveStat = await stat(archivePath);
  if (archiveStat.size !== browser.archiveSize) {
    throw new Error(
      `${archivePath} size ${archiveStat.size} does not match ${browser.archiveSize}`,
    );
  }

  const hash = createHash("sha256");
  await pipeline(createReadStream(archivePath), hash);
  const digest = hash.digest("hex");
  if (digest !== browser.archiveSha256) {
    throw new Error(
      `${archivePath} SHA-256 ${digest} does not match ${browser.archiveSha256}`,
    );
  }
  return { size: archiveStat.size, sha256: digest };
}

export function browserCachePaths(frontendRoot, browser) {
  const archiveName = path.basename(new URL(browser.archiveUrl).pathname);
  const cacheRoot = path.join(frontendRoot, ".cache", "client-browsers");
  const extractionPath = path.join(cacheRoot, "extracted", browser.slot);
  return {
    archivePath: path.join(cacheRoot, "archives", browser.slot, archiveName),
    cacheRoot,
    executablePath: path.join(
      extractionPath,
      ...browser.executableRelativePath.split("/"),
    ),
    extractionPath,
  };
}

export async function downloadArchive(browser, archivePath) {
  const authorized = AUTHORIZED_BROWSER_SLOTS.find(
    ({ slot }) => slot === browser.slot,
  );
  if (!authorized || browser.archiveUrl !== authorized.archiveUrl) {
    throw new Error(`${browser.slot} download URL is not authorized`);
  }
  const url = new URL(browser.archiveUrl);
  if (url.protocol !== "https:") {
    throw new Error(`${browser.slot} download URL must use HTTPS`);
  }

  try {
    await access(archivePath);
    return false;
  } catch {
    // A missing archive is the only condition that permits a download.
  }

  await mkdir(path.dirname(archivePath), { recursive: true });
  const temporaryPath = `${archivePath}.partial-${process.pid}`;
  await rm(temporaryPath, { force: true });
  try {
    const response = await fetch(url, { redirect: "error" });
    if (!response.ok || response.body === null) {
      throw new Error(
        `${browser.slot} download failed with HTTP ${response.status}`,
      );
    }
    await pipeline(
      Readable.fromWeb(response.body),
      createWriteStream(temporaryPath, { flags: "wx" }),
    );
    await verifyArchive(temporaryPath, browser);
    await rename(temporaryPath, archivePath);
    return true;
  } finally {
    await rm(temporaryPath, { force: true });
  }
}

export const repositoryRoot = path.resolve(
  path.dirname(fileURLToPath(import.meta.url)),
  "../..",
);
