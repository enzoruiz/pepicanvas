import { readFile } from "node:fs/promises";

import { AUTHORIZED_OBS_SLOTS } from "./obs-matrix.mjs";

const rootFields = [
  "completedAt",
  "correlationId",
  "installer",
  "obsVersion",
  "outputs",
  "releaseDate",
  "result",
  "runtime",
  "scenarioId",
  "schemaVersion",
  "slot",
  "startedAt",
];
const installerFields = ["sha256", "size", "url"];
const runtimeFields = ["architecture", "node", "os"];
const outputFields = ["name", "sha256", "size"];

function exactFields(value, fields, location) {
  if (value === null || typeof value !== "object" || Array.isArray(value)) {
    throw new Error(`${location} must be an object`);
  }
  const actual = Object.keys(value);
  const unknown = actual.find((field) => !fields.includes(field));
  if (unknown) throw new Error(`${location} contains unknown field ${unknown}`);
  const missing = fields.find((field) => !actual.includes(field));
  if (missing) throw new Error(`${location} is missing field ${missing}`);
}

function validTime(value) {
  return (
    typeof value === "string" &&
    /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,3})?Z$/.test(value) &&
    !Number.isNaN(Date.parse(value))
  );
}

export function validateObsEvidence(value) {
  exactFields(value, rootFields, "OBS evidence");
  const release = AUTHORIZED_OBS_SLOTS.find(({ slot }) => slot === value.slot);
  if (!release) throw new Error(`unsupported OBS evidence slot ${value.slot}`);
  if (value.schemaVersion !== 1)
    throw new Error("OBS evidence schemaVersion must be 1");
  if (value.obsVersion !== release.version)
    throw new Error(`${value.slot} OBS version does not match matrix`);
  if (value.releaseDate !== release.publishedAt.slice(0, 10))
    throw new Error(`${value.slot} release date does not match matrix`);
  if (value.scenarioId !== `obs-browser-source-${release.releaseTrack}`)
    throw new Error(`${value.slot} scenario ID does not match matrix`);
  if (value.result !== "PASS")
    throw new Error(`${value.slot} result must be PASS`);
  if (
    !validTime(value.startedAt) ||
    !validTime(value.completedAt) ||
    Date.parse(value.completedAt) < Date.parse(value.startedAt) ||
    Date.parse(value.startedAt) < Date.parse(release.publishedAt)
  ) {
    throw new Error(`${value.slot} timestamps are invalid`);
  }
  if (
    typeof value.correlationId !== "string" ||
    !/^[A-Za-z0-9][A-Za-z0-9._-]+$/.test(value.correlationId)
  ) {
    throw new Error(`${value.slot} correlation ID is invalid`);
  }

  exactFields(value.installer, installerFields, "installer");
  if (
    value.installer.url !== release.installerUrl ||
    value.installer.size !== release.installerSize ||
    value.installer.sha256 !== release.installerSha256
  ) {
    throw new Error(`${value.slot} installer identity does not match matrix`);
  }
  exactFields(value.runtime, runtimeFields, "runtime");
  if (
    value.runtime.os !== "windows" ||
    value.runtime.architecture !== "x64" ||
    typeof value.runtime.node !== "string" ||
    !/^\d+\.\d+\.\d+$/.test(value.runtime.node)
  ) {
    throw new Error(`${value.slot} runtime identity is invalid`);
  }
  if (!Array.isArray(value.outputs))
    throw new Error(`${value.slot} outputs must be an array`);
  const names = new Set();
  for (const output of value.outputs) {
    exactFields(output, outputFields, "output");
    if (names.has(output.name))
      throw new Error(`${value.slot} contains duplicate output ${output.name}`);
    names.add(output.name);
    if (
      !/^[A-Za-z0-9][A-Za-z0-9._-]+$/.test(output.name) ||
      !/^[0-9a-f]{64}$/.test(output.sha256) ||
      !Number.isSafeInteger(output.size) ||
      output.size <= 0
    ) {
      throw new Error(`${value.slot} output identity is invalid`);
    }
  }
  if (![...names].some((name) => name.endsWith("source-screenshot.png"))) {
    throw new Error(`${value.slot} is missing render proof`);
  }
  return value;
}

export async function loadObsEvidence(path) {
  let value;
  try {
    value = JSON.parse(await readFile(path, "utf8"));
  } catch (error) {
    throw new Error(`OBS evidence is not valid JSON: ${error.message}`, {
      cause: error,
    });
  }
  return validateObsEvidence(value);
}
