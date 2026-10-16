import { readdir } from "node:fs/promises";
import path from "node:path";

import { loadObsEvidence } from "./obs-evidence.mjs";

const [directory, ...rest] = process.argv.slice(2);
if (!directory || rest.length)
  throw new Error("usage: node validate-evidence.mjs <directory>");
const evidence = (await readdir(directory, { recursive: true }))
  .filter((name) => name.endsWith("-evidence.json"))
  .sort();
if (evidence.length !== 2)
  throw new Error(
    `expected exactly two OBS evidence files, found ${evidence.length}`,
  );
const slots = new Set();
for (const name of evidence) {
  const value = await loadObsEvidence(path.join(directory, name));
  if (slots.has(value.slot))
    throw new Error(`duplicate evidence slot ${value.slot}`);
  slots.add(value.slot);
  console.log(`${value.slot}: ${value.result} (${value.obsVersion})`);
}
if (!slots.has("obs-current") || !slots.has("obs-previous"))
  throw new Error("OBS evidence is missing a required slot");
