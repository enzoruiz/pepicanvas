import { spawnSync } from "node:child_process";
import { mkdir, readdir, rename, rm, writeFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  acquireInstaller,
  installerPath,
  loadObsMatrix,
  verifyInstaller,
} from "./obs-matrix.mjs";

export const repositoryRoot = path.resolve(import.meta.dirname, "../..");
export const cacheRoot = path.join(import.meta.dirname, ".cache");

async function findFile(root, name) {
  for (const entry of await readdir(root, { withFileTypes: true })) {
    const candidate = path.join(root, entry.name);
    if (entry.isFile() && entry.name.toLowerCase() === name.toLowerCase())
      return candidate;
    if (entry.isDirectory()) {
      const found = await findFile(candidate, name);
      if (found) return found;
    }
  }
  return undefined;
}

function run(command, args) {
  const result = spawnSync(command, args, {
    encoding: "utf8",
    stdio: ["ignore", "pipe", "pipe"],
  });
  if (result.error || result.status !== 0) {
    throw new Error(
      `${command} failed: ${result.stderr?.trim() || result.error?.message || result.status}`,
    );
  }
}

export async function prepareInstaller(release, { offline }) {
  const target = installerPath(cacheRoot, release);
  return {
    path: target,
    verified: await acquireInstaller(release, target, { offline }),
  };
}

export async function extractObs(release, installer) {
  if (process.platform !== "win32") {
    throw new Error(
      "Windows OBS extraction is pending; Linux never claims OBS execution",
    );
  }
  await verifyInstaller(installer, release);
  const extraction = path.join(cacheRoot, "extracted", release.slot);
  const temporary = `${extraction}.partial-${process.pid}`;
  await rm(temporary, { force: true, recursive: true });
  await mkdir(temporary, { recursive: true });
  try {
    run("7z.exe", ["x", "-y", `-o${temporary}`, installer]);
    const executable = await findFile(temporary, "obs64.exe");
    if (!executable)
      throw new Error(`${release.slot} extraction did not contain obs64.exe`);
    await writeFile(path.join(obsRoot(executable), "portable_mode.txt"), "");
    await rm(extraction, { force: true, recursive: true });
    await mkdir(path.dirname(extraction), { recursive: true });
    await rename(temporary, extraction);
  } finally {
    await rm(temporary, { force: true, recursive: true });
  }
  const executable = await findFile(extraction, "obs64.exe");
  if (!executable)
    throw new Error(`${release.slot} executable disappeared after extraction`);
  return { executable, root: obsRoot(executable) };
}

function obsRoot(executable) {
  const executableDirectory = path.dirname(executable);
  if (
    path.basename(executableDirectory).toLowerCase() !== "64bit" ||
    path.basename(path.dirname(executableDirectory)).toLowerCase() !== "bin"
  ) {
    throw new Error("obs64.exe is outside the expected bin/64bit layout");
  }
  return path.dirname(path.dirname(executableDirectory));
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length || !["--download", "--offline"].includes(mode)) {
    throw new Error("usage: node obs-cache.mjs --download|--offline");
  }
  const matrix = await loadObsMatrix(
    path.join(repositoryRoot, "releases/obs-browser-source-matrix.json"),
  );
  for (const release of matrix.releases) {
    const prepared = await prepareInstaller(release, {
      offline: mode === "--offline",
    });
    console.log(
      `${release.slot}: ${prepared.verified.size} bytes; sha256:${prepared.verified.sha256}`,
    );
  }
  if (process.platform !== "win32")
    console.log("Windows OBS execution: PENDING (verification only on Linux)");
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
