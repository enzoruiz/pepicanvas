import { createHash } from "node:crypto";
import { execFile } from "node:child_process";
import { readdir, readFile, writeFile } from "node:fs/promises";
import path from "node:path";
import { promisify } from "node:util";
import { fileURLToPath } from "node:url";

import { validateObsEvidence } from "./obs-evidence.mjs";

const execFileAsync = promisify(execFile);

async function gitOutput(cwd, args, message) {
  try {
    return (await execFileAsync("git", args, { cwd, encoding: "buffer" }))
      .stdout;
  } catch (error) {
    throw new Error(message, { cause: error });
  }
}

function sha256(data) {
  return createHash("sha256").update(data).digest("hex");
}

export function validateBrowserEvidence(value, manifest) {
  const fields = [
    "browserVersion",
    "completedAt",
    "correlationId",
    "outputs",
    "releaseDate",
    "result",
    "runtime",
    "scenarioId",
    "schemaVersion",
    "slot",
    "startedAt",
  ];
  if (value === null || typeof value !== "object" || Array.isArray(value))
    throw new Error("browser evidence must be an object");
  const actual = Object.keys(value);
  const unknown = actual.find((field) => !fields.includes(field));
  const missing = fields.find((field) => !actual.includes(field));
  if (unknown || missing)
    throw new Error(
      `browser evidence has ${unknown ? `unknown field ${unknown}` : `missing field ${missing}`}`,
    );
  const [family, slot] = String(value.slot).split("-");
  const client = manifest.clients.find(
    (candidate) => candidate.family === family && candidate.slot === slot,
  );
  const scenario = manifest.scenarios.find(
    (candidate) => candidate.id === value.scenarioId,
  );
  if (
    !client ||
    !["chrome", "edge"].includes(family) ||
    value.schemaVersion !== 1 ||
    value.browserVersion !== client.version ||
    value.releaseDate !== client.releaseDate ||
    !scenario ||
    scenario.clientFamily !== family ||
    scenario.clientSlot !== slot ||
    value.result !== "PASS"
  ) {
    throw new Error(
      `${value.slot} browser evidence identity does not match the release template`,
    );
  }
  const runtimeFields = ["architecture", "node", "os", "playwright"];
  if (
    value.runtime === null ||
    typeof value.runtime !== "object" ||
    Array.isArray(value.runtime) ||
    Object.keys(value.runtime).sort().join() !== runtimeFields.sort().join() ||
    value.runtime.os !== "linux" ||
    value.runtime.architecture !== "x64"
  ) {
    throw new Error(`${value.slot} browser evidence runtime is invalid`);
  }
  if (
    !Array.isArray(value.outputs) ||
    value.outputs.length === 0 ||
    value.outputs.some(
      (output) =>
        output === null ||
        typeof output !== "object" ||
        Array.isArray(output) ||
        Object.keys(output).sort().join() !== "name,sha256,size" ||
        !/^[A-Za-z0-9][A-Za-z0-9._-]+$/.test(output.name) ||
        !/^[0-9a-f]{64}$/.test(output.sha256) ||
        !Number.isSafeInteger(output.size) ||
        output.size <= 0,
    )
  ) {
    throw new Error(`${value.slot} browser evidence outputs are invalid`);
  }
  if (
    Number.isNaN(Date.parse(value.startedAt)) ||
    Number.isNaN(Date.parse(value.completedAt)) ||
    Date.parse(value.completedAt) < Date.parse(value.startedAt) ||
    !/^[A-Za-z0-9][A-Za-z0-9._-]+$/.test(value.correlationId)
  ) {
    throw new Error(
      `${value.slot} browser evidence chronology or correlation is invalid`,
    );
  }
  return value;
}

export async function generateReleaseManifest({
  templatePath,
  evidenceDirectory,
  repositoryUrl,
  evidenceCommit,
}) {
  const manifest = JSON.parse(await readFile(templatePath, "utf8"));
  const files = (await readdir(evidenceDirectory, { withFileTypes: true }))
    .filter((entry) => entry.isFile() && entry.name.endsWith(".json"))
    .map((entry) => entry.name)
    .sort();
  const gitRoot = path.resolve(path.dirname(templatePath), "..");

  if (files.length) {
    if (!repositoryUrl || !evidenceCommit) {
      throw new Error(
        "repositoryUrl and evidenceCommit are required when evidence is present",
      );
    }
    if (!/^[0-9a-f]{40}$/.test(evidenceCommit))
      throw new Error("evidenceCommit must be a full Git commit SHA");
    await gitOutput(
      gitRoot,
      ["cat-file", "-e", `${evidenceCommit}^{commit}`],
      "evidenceCommit must identify an existing local Git commit",
    );
  }

  for (const name of files) {
    const bytes = await readFile(path.join(evidenceDirectory, name));
    const value = JSON.parse(bytes);
    if (String(value.slot ?? "").startsWith("obs-")) validateObsEvidence(value);
    else validateBrowserEvidence(value, manifest);
    const committed = await gitOutput(
      gitRoot,
      ["show", `${evidenceCommit}:releases/evidence/${name}`],
      `committed evidence ${name} is missing`,
    );
    if (!Buffer.from(committed).equals(bytes))
      throw new Error(`committed evidence ${name} does not match local bytes`);
    const scenario = manifest.scenarios.find(
      ({ id }) => id === value.scenarioId,
    );
    if (!scenario)
      throw new Error(`no release scenario matches ${value.scenarioId}`);
    const id = `evidence-${value.scenarioId}`;
    scenario.evidence.push(id);
    manifest.evidence.push({
      id,
      releaseVersion: manifest.release.version,
      scenarioId: value.scenarioId,
      location: `${repositoryUrl.replace(/\/$/, "")}/blob/${evidenceCommit}/releases/evidence/${encodeURIComponent(name)}`,
      sha256: sha256(bytes),
      correlationId: value.correlationId,
      startedAt: value.startedAt,
      completedAt: value.completedAt,
      result: value.result,
      accessPolicy: "public-repository-readers-v1",
      retentionPolicy: "git-history-retained-v1",
    });
  }
  return manifest;
}

async function main() {
  const args = process.argv.slice(2);
  if (args.length !== 5) {
    throw new Error(
      "usage: node release-manifest.mjs <template> <evidence-dir> <repository-url> <evidence-commit> <output>",
    );
  }
  const [
    templatePath,
    evidenceDirectory,
    repositoryUrl,
    evidenceCommit,
    output,
  ] = args;
  const manifest = await generateReleaseManifest({
    templatePath,
    evidenceDirectory,
    repositoryUrl,
    evidenceCommit,
  });
  await writeFile(output, `${JSON.stringify(manifest, null, 2)}\n`, {
    flag: "wx",
  });
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
