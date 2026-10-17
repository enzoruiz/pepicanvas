import { createHash } from "node:crypto";
import { createReadStream, createWriteStream } from "node:fs";
import { access, mkdir, readFile, rename, rm, stat } from "node:fs/promises";
import path from "node:path";
import { Readable } from "node:stream";
import { pipeline } from "node:stream/promises";

export const AUTHORIZED_OBS_SLOTS = Object.freeze([
  {
    slot: "obs-current",
    releaseTrack: "current",
    version: "32.2.2",
    tag: "32.2.2",
    publishedAt: "2026-08-14T22:43:31Z",
    platform: "windows",
    architecture: "x64",
    releaseUrl: "https://github.com/obsproject/obs-studio/releases/tag/32.2.2",
    releaseApiUrl:
      "https://api.github.com/repos/obsproject/obs-studio/releases/tags/32.2.2",
    assetApiUrl:
      "https://api.github.com/repos/obsproject/obs-studio/releases/assets/515013677",
    installerName: "OBS-Studio-32.2.2-Windows-x64-Installer.exe",
    installerUrl:
      "https://github.com/obsproject/obs-studio/releases/download/32.2.2/OBS-Studio-32.2.2-Windows-x64-Installer.exe",
    installerSize: 159809104,
    installerSha256:
      "c3a0b880adbe64dc4bcb68f93016916ab5b55ae43fd227115287bf80257d92dc",
    hashProvenance: "official",
    sizeProvenance: "official",
  },
  {
    slot: "obs-previous",
    releaseTrack: "previous",
    version: "32.2.1",
    tag: "32.2.1",
    publishedAt: "2026-07-24T22:58:26Z",
    platform: "windows",
    architecture: "x64",
    releaseUrl: "https://github.com/obsproject/obs-studio/releases/tag/32.2.1",
    releaseApiUrl:
      "https://api.github.com/repos/obsproject/obs-studio/releases/tags/32.2.1",
    assetApiUrl:
      "https://api.github.com/repos/obsproject/obs-studio/releases/assets/488910690",
    installerName: "OBS-Studio-32.2.1-Windows-x64-Installer.exe",
    installerUrl:
      "https://github.com/obsproject/obs-studio/releases/download/32.2.1/OBS-Studio-32.2.1-Windows-x64-Installer.exe",
    installerSize: 159828560,
    installerSha256:
      "bbb95e52b96ad9b7ccd5abd13121379d29774d6cc5fdbef82ffa249e8a24a289",
    hashProvenance: "official",
    sizeProvenance: "official",
  },
]);

const rootFields = ["architecture", "platform", "releases", "schemaVersion"];
const releaseFields = Object.keys(AUTHORIZED_OBS_SLOTS[0]);
const allowedDownloadHosts = new Set([
  "github.com",
  "objects.githubusercontent.com",
  "release-assets.githubusercontent.com",
]);

function assertExactFields(value, expected, location) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${location} must be an object`);
  }
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  const unknown = actual.find((field) => !wanted.includes(field));
  if (unknown) throw new Error(`${location} contains unknown field ${unknown}`);
  const missing = wanted.find((field) => !actual.includes(field));
  if (missing) throw new Error(`${location} is missing field ${missing}`);
}

function assertHTTPS(value, location) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new Error(`${location} must be an absolute HTTPS URL`);
  }
  if (
    url.protocol !== "https:" ||
    !url.hostname ||
    url.username ||
    url.password
  ) {
    throw new Error(`${location} must be an absolute HTTPS URL`);
  }
}

export async function loadObsMatrix(matrixPath) {
  let matrix;
  try {
    matrix = JSON.parse(await readFile(matrixPath, "utf8"));
  } catch (error) {
    throw new Error(`OBS matrix is not valid JSON: ${error.message}`, {
      cause: error,
    });
  }
  assertExactFields(matrix, rootFields, "OBS matrix");
  if (
    matrix.schemaVersion !== 1 ||
    matrix.platform !== "windows" ||
    matrix.architecture !== "x64"
  ) {
    throw new Error("OBS matrix root contract does not match authorization");
  }
  if (!Array.isArray(matrix.releases) || matrix.releases.length !== 2) {
    throw new Error("OBS matrix must contain exactly two releases");
  }
  const slots = new Set();
  matrix.releases.forEach((release, index) => {
    assertExactFields(release, releaseFields, `releases[${index}]`);
    const expected = AUTHORIZED_OBS_SLOTS[index];
    for (const field of releaseFields) {
      if (release[field] !== expected[field]) {
        throw new Error(
          `${release.slot ?? `releases[${index}]`} does not match authorized ${field}`,
        );
      }
    }
    for (const field of [
      "releaseUrl",
      "releaseApiUrl",
      "assetApiUrl",
      "installerUrl",
    ]) {
      assertHTTPS(release[field], `${release.slot} ${field}`);
    }
    if (
      !/^[0-9a-f]{64}$/.test(release.installerSha256) ||
      release.installerSize <= 0
    ) {
      throw new Error(`${release.slot} has an invalid installer hash or size`);
    }
    if (
      !new Set(["official", "observed-bootstrap"]).has(
        release.hashProvenance,
      ) ||
      !new Set(["official", "observed-bootstrap"]).has(release.sizeProvenance)
    ) {
      throw new Error(`${release.slot} has unsupported provenance`);
    }
    if (slots.has(release.slot))
      throw new Error(`OBS matrix contains duplicate slot ${release.slot}`);
    slots.add(release.slot);
  });
  return matrix;
}

export async function verifyInstaller(installerPath, release) {
  const fileStat = await stat(installerPath);
  if (!fileStat.isFile() || fileStat.size !== release.installerSize) {
    throw new Error(
      `${installerPath} size ${fileStat.size} does not match ${release.installerSize}`,
    );
  }
  const hash = createHash("sha256");
  await pipeline(createReadStream(installerPath), hash);
  const sha256 = hash.digest("hex");
  if (sha256 !== release.installerSha256) {
    throw new Error(
      `${installerPath} SHA-256 ${sha256} does not match ${release.installerSha256}`,
    );
  }
  return { size: fileStat.size, sha256 };
}

export function installerPath(cacheRoot, release) {
  return path.join(
    cacheRoot,
    "installers",
    release.slot,
    release.installerName,
  );
}

export async function acquireInstaller(
  release,
  destination,
  { offline = false } = {},
) {
  const authorized = AUTHORIZED_OBS_SLOTS.find(
    ({ slot }) => slot === release.slot,
  );
  if (!authorized || release.installerUrl !== authorized.installerUrl) {
    throw new Error(`${release.slot} installer URL is not authorized`);
  }
  try {
    return await verifyInstaller(destination, release);
  } catch (error) {
    try {
      await access(destination);
      throw error;
    } catch (accessError) {
      if (accessError === error) throw error;
    }
  }
  if (offline)
    throw new Error(`${release.slot} installer is absent in offline mode`);

  await mkdir(path.dirname(destination), { recursive: true });
  const temporary = `${destination}.partial-${process.pid}`;
  await rm(temporary, { force: true });
  try {
    const response = await fetch(release.installerUrl, { redirect: "follow" });
    const finalUrl = new URL(response.url);
    if (
      !response.ok ||
      response.body === null ||
      finalUrl.protocol !== "https:" ||
      !allowedDownloadHosts.has(finalUrl.hostname)
    ) {
      throw new Error(
        `${release.slot} download failed or redirected outside authorized GitHub hosts`,
      );
    }
    await pipeline(
      Readable.fromWeb(response.body),
      createWriteStream(temporary, { flags: "wx" }),
    );
    const verified = await verifyInstaller(temporary, release);
    await rename(temporary, destination);
    return verified;
  } finally {
    await rm(temporary, { force: true });
  }
}
