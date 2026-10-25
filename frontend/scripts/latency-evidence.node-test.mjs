import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import test from "node:test";

import {
  assembleEvidence,
  buildVersions,
  commandPlan,
  describeExit,
  parseGoModRequires,
  parseLockPackages,
  transmissionId,
} from "./latency-evidence.mjs";

const goMod = `module example.com/app

go 1.26.0

require example.com/single v1.0.0

require (
	github.com/b/two v2.0.0
	github.com/a/one v1.2.3 // indirect
	"example.com/quoted" v0.1.0

	// a comment line
)

require example.com/also v0.0.1 // indirect
`;

const lock = JSON.stringify({
  name: "app",
  lockfileVersion: 3,
  packages: {
    "": { name: "app", version: "0.0.0" },
    "node_modules/react": { version: "19.3.0" },
    "node_modules/@scope/pkg": { version: "1.0.0" },
    "node_modules/a/node_modules/react": { version: "18.0.0" },
    "node_modules/b/node_modules/react": { version: "18.0.0" },
    "node_modules/a": { version: "2.0.0" },
  },
});

const manifest = JSON.stringify({
  release: { version: "0.1.0" },
  clients: [
    { family: "chrome", slot: "current", version: "155", releaseDate: "x" },
    { family: "obs", slot: "previous", version: "32.2.1", releaseDate: "y" },
  ],
  toolchain: {
    go: { name: "go", version: "1.27.2" },
    node: { name: "node", version: "24.16.0" },
    packageManager: { name: "npm", version: "11.13.0" },
    postgresql: { name: "postgresql", version: "17.4" },
    s3CompatibleStorage: { name: "s3-compatible", version: "1.0.0-contract" },
    ffmpeg: { name: "ffmpeg", version: "7.1.1" },
    ffprobe: { name: "ffprobe", version: "7.1.2" },
  },
});

test("parseGoModRequires reads single-line and block requires, direct and indirect, sorted", () => {
  assert.deepEqual(parseGoModRequires(goMod), [
    { name: "example.com/also", version: "v0.0.1" },
    { name: "example.com/quoted", version: "v0.1.0" },
    { name: "example.com/single", version: "v1.0.0" },
    { name: "github.com/a/one", version: "v1.2.3" },
    { name: "github.com/b/two", version: "v2.0.0" },
  ]);
});

test("parseGoModRequires rejects malformed entries and unterminated blocks", () => {
  assert.throws(
    () => parseGoModRequires("require (\n\ta/b\n)\n"),
    /line 2: malformed require entry/,
  );
  assert.throws(
    () => parseGoModRequires("require (\n\ta/b v1.0.0\n"),
    /unterminated require block/,
  );
});

test("parseLockPackages names by the last node_modules/, skips the root, keeps duplicates once, sorted", () => {
  assert.deepEqual(parseLockPackages(lock), [
    { name: "@scope/pkg", version: "1.0.0" },
    { name: "a", version: "2.0.0" },
    { name: "react", version: "18.0.0" },
    { name: "react", version: "19.3.0" },
  ]);
});

test("parseLockPackages rejects empty packages and entries without version", () => {
  assert.throws(() => parseLockPackages("{}"), /packages must not be empty/);
  assert.throws(
    () =>
      parseLockPackages(JSON.stringify({ packages: { "node_modules/x": {} } })),
    /node_modules\/x has no version/,
  );
});

test("buildVersions takes observed values and the manifest-only pins", () => {
  const versions = buildVersions({
    manifestText: manifest,
    goVersion: "go1.27.2\n",
    nodeVersion: "24.16.0",
    npmVersion: "11.13.0\n",
    goModText: goMod,
    lockText: lock,
  });
  assert.deepEqual(versions, {
    releaseManifestSha256: createHash("sha256").update(manifest).digest("hex"),
    release: "0.1.0",
    go: "1.27.2",
    node: "24.16.0",
    packageManager: { name: "npm", version: "11.13.0" },
    goModules: parseGoModRequires(goMod),
    frontendDependencies: parseLockPackages(lock),
    postgresql: "17.4",
    s3CompatibleStorage: "1.0.0-contract",
    ffmpeg: "7.1.1",
    ffprobe: "7.1.2",
    clients: [
      { family: "chrome", slot: "current", version: "155" },
      { family: "obs", slot: "previous", version: "32.2.1" },
    ],
  });
});

test("buildVersions keeps an observed Go that differs from the manifest", () => {
  const versions = buildVersions({
    manifestText: manifest,
    goVersion: "go1.26.0",
    nodeVersion: "24.15.0",
    npmVersion: "11.0.0",
    goModText: goMod,
    lockText: lock,
  });
  assert.equal(versions.go, "1.26.0");
  assert.equal(versions.node, "24.15.0");
  assert.deepEqual(versions.packageManager, { name: "npm", version: "11.0.0" });
});

test("buildVersions rejects a Go version without the go prefix", () => {
  assert.throws(
    () =>
      buildVersions({
        manifestText: manifest,
        goVersion: "1.27.2",
        nodeVersion: "24.16.0",
        npmVersion: "11.13.0",
        goModText: goMod,
        lockText: lock,
      }),
    /unexpected go env GOVERSION output/,
  );
});

test("transmissionId uses the slot and the UTC second", () => {
  assert.equal(
    transmissionId("chrome-current", new Date("2026-10-24T09:05:07.912Z")),
    "chrome-current-20261024T090507Z",
  );
});

test("commandPlan runs create, move, resize, rotate, media, delete per cycle with alternating media", () => {
  const plan = commandPlan(20);
  assert.equal(plan.length, 120);
  assert.deepEqual(
    plan.slice(0, 6).map((step) => step.kind),
    ["create", "move", "resize", "rotate", "media", "delete"],
  );
  assert.deepEqual(plan[0], {
    kind: "create",
    objectId: "box-1",
    command: "set",
    payload: {
      op: "create",
      x: 40,
      y: 40,
      width: 160,
      height: 90,
      rotation: 0,
    },
  });
  assert.deepEqual(plan[4], {
    kind: "media",
    objectId: "media-1",
    command: "set",
    payload: { op: "media", action: "play" },
  });
  assert.deepEqual(plan[5], {
    kind: "delete",
    objectId: "box-1",
    command: "delete",
  });
  assert.equal(plan[6].objectId, "box-2");
  assert.deepEqual(plan[10].payload, { op: "media", action: "pause" });
  assert.deepEqual(plan[16].payload, { op: "media", action: "play" });
  for (const kind of [
    "create",
    "move",
    "resize",
    "rotate",
    "media",
    "delete",
  ]) {
    assert.equal(plan.filter((step) => step.kind === kind).length, 20);
  }
});

test("assembleEvidence builds the schema object in schema order", () => {
  const transmission = {
    id: "chrome-current-20261024T090507Z",
    scenarioId: "remote-render-latency",
    correlationId: "0123456789abcdef0123456789abcdef",
    startedAt: "2026-10-24T09:05:07.912Z",
    completedAt: "2026-10-24T09:05:37.000Z",
    measurementStartNs: 5,
    client: { family: "chrome", slot: "current", version: "155" },
  };
  const versions = { release: "0.1.0" };
  const samples = [{ id: "s-001" }];
  const evidence = assembleEvidence({ transmission, versions, samples });
  assert.deepEqual(Object.keys(evidence), [
    "schemaVersion",
    "transmission",
    "versions",
    "samples",
  ]);
  assert.equal(evidence.schemaVersion, 1);
  assert.deepEqual(Object.keys(evidence.transmission), [
    "id",
    "scenarioId",
    "correlationId",
    "startedAt",
    "completedAt",
    "measurementStartNs",
    "client",
  ]);
  assert.deepEqual(evidence.transmission, transmission);
  assert.equal(evidence.versions, versions);
  assert.equal(evidence.samples, samples);
});

test("describeExit names how a child process ended", () => {
  assert.equal(describeExit({ status: 1, signal: null }), "exit 1");
  assert.equal(
    describeExit({ status: null, signal: "SIGKILL" }),
    "signal SIGKILL",
  );
  assert.equal(
    describeExit({
      status: null,
      signal: null,
      error: new Error("spawn go ENOENT"),
    }),
    "spawn go ENOENT",
  );
  assert.equal(describeExit({ status: null, signal: null }), "unknown");
});
