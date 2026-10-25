// Pure helpers for the TE-010 remote render latency evidence: the command
// plan, the transmission identifier, the observed versions block and the
// evidence object. The go.mod and package-lock readers build exactly what
// backend/cmd/latencycheck compares against, so a matching repository yields
// matching pins.

import { createHash } from "node:crypto";

export const scenarioId = "remote-render-latency";

const kinds = ["create", "move", "resize", "rotate", "media", "delete"];

function compareComponents(a, b) {
  if (a.name !== b.name) return a.name < b.name ? -1 : 1;
  if (a.version !== b.version) return a.version < b.version ? -1 : 1;
  return 0;
}

// goUnquote mirrors strconv.Unquote for the double-quoted module paths go.mod
// allows; anything else is kept as written.
function goUnquote(value) {
  if (value.length >= 2 && value.startsWith('"') && value.endsWith('"')) {
    try {
      return JSON.parse(value);
    } catch {
      return value;
    }
  }
  return value;
}

// parseGoModRequires returns every require entry of go.mod, direct and
// indirect, in single-line and block form, sorted by name then version.
export function parseGoModRequires(text) {
  const modules = [];
  let inBlock = false;
  text.split("\n").forEach((raw, index) => {
    const comment = raw.indexOf("//");
    const line = comment >= 0 ? raw.slice(0, comment) : raw;
    let fields = line.trim().split(/\s+/).filter(Boolean);
    if (fields.length === 0) return;
    if (inBlock) {
      if (fields.length === 1 && fields[0] === ")") {
        inBlock = false;
        return;
      }
    } else if (fields[0] !== "require") {
      return;
    } else if (fields.length === 2 && fields[1] === "(") {
      inBlock = true;
      return;
    } else {
      fields = fields.slice(1);
    }
    if (fields.length !== 2) {
      throw new Error(`line ${index + 1}: malformed require entry`);
    }
    modules.push({ name: goUnquote(fields[0]), version: fields[1] });
  });
  if (inBlock) {
    throw new Error("unterminated require block");
  }
  return modules.sort(compareComponents);
}

// parseLockPackages returns every non-root packages entry of package-lock,
// named by the path after the last node_modules/, sorted by name then
// version with duplicates once. A "link": true entry has no version of its
// own and is skipped; the package it links to has its own entry.
export function parseLockPackages(text) {
  const packages = JSON.parse(text).packages ?? {};
  if (Object.keys(packages).length === 0) {
    throw new Error("packages must not be empty");
  }
  const seen = new Set();
  const result = [];
  for (const [entryPath, entry] of Object.entries(packages)) {
    if (entryPath === "" || entry?.link === true) continue;
    if (!entry?.version) {
      throw new Error(`${entryPath} has no version`);
    }
    const marker = entryPath.lastIndexOf("node_modules/");
    const name =
      marker >= 0
        ? entryPath.slice(marker + "node_modules/".length)
        : entryPath;
    const key = JSON.stringify([name, entry.version]);
    if (!seen.has(key)) {
      seen.add(key);
      result.push({ name, version: entry.version });
    }
  }
  return result.sort(compareComponents);
}

// buildVersions records what this run observed: the release manifest bytes,
// the Go toolchain that ran the harness (`go env GOVERSION`), this Node.js,
// this npm, go.mod and package-lock.json.
export function buildVersions({
  manifestText,
  goVersion,
  nodeVersion,
  npmVersion,
  goModText,
  lockText,
}) {
  const observedGo = goVersion.trim();
  if (!/^go\d/.test(observedGo)) {
    throw new Error(`unexpected go env GOVERSION output: ${observedGo}`);
  }
  const manifest = JSON.parse(manifestText);
  const toolchain = manifest.toolchain;
  return {
    releaseManifestSha256: createHash("sha256")
      .update(manifestText)
      .digest("hex"),
    release: manifest.release.version,
    go: observedGo.slice("go".length),
    node: nodeVersion,
    packageManager: { name: "npm", version: npmVersion.trim() },
    goModules: parseGoModRequires(goModText),
    frontendDependencies: parseLockPackages(lockText),
    // This harness runs no PostgreSQL, S3 storage, ffmpeg or ffprobe and
    // measures one browser per transmission, so these pins cannot be
    // observed here and are taken from the release manifest.
    postgresql: toolchain.postgresql.version,
    s3CompatibleStorage: toolchain.s3CompatibleStorage.version,
    ffmpeg: toolchain.ffmpeg.version,
    ffprobe: toolchain.ffprobe.version,
    clients: manifest.clients.map(({ family, slot, version }) => ({
      family,
      slot,
      version,
    })),
  };
}

// transmissionId is `<slot>-<UTC yyyymmddThhmmssZ>`.
export function transmissionId(slot, date) {
  const stamp = date
    .toISOString()
    .replace(/\.\d{3}Z$/, "Z")
    .replace(/[-:]/g, "");
  return `${slot}-${stamp}`;
}

// commandPlan returns cycles × [create, move, resize, rotate, media, delete].
// Each cycle uses its own box; media alternates play and pause on media-1,
// starting with play.
export function commandPlan(cycles) {
  const plan = [];
  for (let cycle = 0; cycle < cycles; cycle++) {
    const objectId = `box-${cycle + 1}`;
    const box = (payload) => ({ objectId, command: "set", payload });
    const steps = {
      create: box({
        op: "create",
        x: 40,
        y: 40,
        width: 160,
        height: 90,
        rotation: 0,
      }),
      move: box({ op: "move", x: 320, y: 200 }),
      resize: box({ op: "resize", width: 240, height: 135 }),
      rotate: box({ op: "rotate", rotation: 45 }),
      media: {
        objectId: "media-1",
        command: "set",
        payload: { op: "media", action: cycle % 2 === 0 ? "play" : "pause" },
      },
      delete: { objectId, command: "delete" },
    };
    for (const kind of kinds) {
      plan.push({ kind, ...steps[kind] });
    }
  }
  return plan;
}

// assembleEvidence builds one transmission's evidence in schema order.
export function assembleEvidence({ transmission, versions, samples }) {
  return {
    schemaVersion: 1,
    transmission: {
      id: transmission.id,
      scenarioId: transmission.scenarioId,
      correlationId: transmission.correlationId,
      startedAt: transmission.startedAt,
      completedAt: transmission.completedAt,
      measurementStartNs: transmission.measurementStartNs,
      client: transmission.client,
    },
    versions,
    samples,
  };
}

// describeExit names how a spawnSync child ended: a killed child has a null
// status and no error, so neither can be assumed.
export function describeExit({ status, signal, error }) {
  if (error) return error.message;
  if (signal) return `signal ${signal}`;
  if (status !== null && status !== undefined) return `exit ${status}`;
  return "unknown";
}
