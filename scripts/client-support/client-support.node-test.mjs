import assert from "node:assert/strict";
import { createHash } from "node:crypto";
import { execFileSync, spawn } from "node:child_process";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  acquireInstaller,
  AUTHORIZED_OBS_SLOTS,
  loadObsMatrix,
  verifyInstaller,
} from "./obs-matrix.mjs";
import { loadObsEvidence, validateObsEvidence } from "./obs-evidence.mjs";
import { executeObsSlot, waitForDiagnostic } from "./obs-harness.mjs";
import { generateReleaseManifest } from "./release-manifest.mjs";

const repositoryRoot = path.resolve(import.meta.dirname, "../..");
const matrixPath = path.join(
  repositoryRoot,
  "releases/obs-browser-source-matrix.json",
);
const templatePath = path.join(
  repositoryRoot,
  "releases/client-support-release.template.json",
);
const diagnosticPath = "/__pepicanvas/obs-client-snapshot";

async function availablePort() {
  const server = net.createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const port = address.port;
  await new Promise((resolve, reject) =>
    server.close((error) => (error ? reject(error) : resolve())),
  );
  return port;
}

async function startStaticServer(root) {
  const port = await availablePort();
  const child = spawn(
    process.execPath,
    [path.join(import.meta.dirname, "static-server.mjs"), root, String(port)],
    { stdio: ["ignore", "pipe", "pipe"] },
  );
  let output = "";
  child.stdout.setEncoding("utf8");
  child.stderr.setEncoding("utf8");
  child.stdout.on("data", (chunk) => (output += chunk));
  child.stderr.on("data", (chunk) => (output += chunk));
  const baseUrl = `http://127.0.0.1:${port}`;
  for (let attempt = 0; attempt < 40; attempt++) {
    if (child.exitCode !== null)
      throw new Error(`static server exited early: ${output}`);
    try {
      const response = await fetch(`${baseUrl}/overlay/`);
      if (response.ok) return { baseUrl, child, output: () => output };
    } catch {
      // Readiness retries are bounded by the loop.
    }
    await new Promise((resolve) => setTimeout(resolve, 25));
  }
  child.kill();
  throw new Error(`static server readiness timed out: ${output}`);
}

async function stopStaticServer(child) {
  if (child.exitCode !== null) return;
  const exited = new Promise((resolve) => child.once("exit", resolve));
  child.kill();
  await exited;
}

async function temporaryJSON(value) {
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-obs-test-"),
  );
  const file = path.join(directory, "value.json");
  await writeFile(file, JSON.stringify(value));
  return file;
}

test("checked-in matrix contains exactly the authorized Windows x64 slots", async () => {
  const matrix = await loadObsMatrix(matrixPath);
  assert.deepEqual(
    matrix.releases.map(({ slot, version }) => ({ slot, version })),
    AUTHORIZED_OBS_SLOTS.map(({ slot, version }) => ({ slot, version })),
  );
  assert.equal(matrix.platform, "windows");
  assert.equal(matrix.architecture, "x64");
});

test("matrix rejects unknown fields, duplicate slots, and identity drift", async () => {
  const source = JSON.parse(await readFile(matrixPath, "utf8"));
  await assert.rejects(
    loadObsMatrix(await temporaryJSON({ ...source, unexpected: true })),
    /unknown field/i,
  );

  source.releases[1].slot = source.releases[0].slot;
  await assert.rejects(
    loadObsMatrix(await temporaryJSON(source)),
    /slot|authorized/i,
  );

  const drift = JSON.parse(await readFile(matrixPath, "utf8"));
  drift.releases[0].installerUrl = "https://example.invalid/OBS.exe";
  await assert.rejects(
    loadObsMatrix(await temporaryJSON(drift)),
    /installerUrl|authorized/i,
  );
});

test("installer verification rejects size and SHA-256 mismatches", async () => {
  const file = await temporaryJSON({ not: "an installer" });
  await assert.rejects(
    verifyInstaller(file, {
      installerSize: 1,
      installerSha256: "0".repeat(64),
    }),
    /size/i,
  );
  const size = (await readFile(file)).length;
  await assert.rejects(
    verifyInstaller(file, {
      installerSize: size,
      installerSha256: "0".repeat(64),
    }),
    /SHA-256/i,
  );
});

test("installer acquisition rejects URL drift before network access", async () => {
  const release = {
    ...AUTHORIZED_OBS_SLOTS[0],
    installerUrl: "https://example.invalid/OBS.exe",
  };
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-download-test-"),
  );
  await assert.rejects(
    acquireInstaller(release, path.join(directory, "OBS.exe")),
    /not authorized/i,
  );
});

test("evidence rejects malformed data, wrong versions, and missing render proof", async () => {
  assert.throws(() => validateObsEvidence({}), /field|object/i);
  const valid = {
    schemaVersion: 1,
    slot: "obs-current",
    obsVersion: AUTHORIZED_OBS_SLOTS[0].version,
    releaseDate: AUTHORIZED_OBS_SLOTS[0].publishedAt.slice(0, 10),
    installer: {
      url: AUTHORIZED_OBS_SLOTS[0].installerUrl,
      size: AUTHORIZED_OBS_SLOTS[0].installerSize,
      sha256: AUTHORIZED_OBS_SLOTS[0].installerSha256,
    },
    scenarioId: "obs-browser-source-current",
    result: "PASS",
    startedAt: "2026-10-15T10:00:00Z",
    completedAt: "2026-10-15T10:01:00Z",
    correlationId: "obs-current-test-run",
    runtime: { os: "windows", architecture: "x64", node: "24.16.0" },
    outputs: [
      { name: "source-screenshot.png", sha256: "a".repeat(64), size: 100 },
    ],
  };
  assert.throws(
    () => validateObsEvidence({ ...valid, obsVersion: "0.0.0" }),
    /version/i,
  );
  assert.throws(
    () => validateObsEvidence({ ...valid, outputs: [] }),
    /render proof/i,
  );
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-evidence-"),
  );
  const bytes = Buffer.from("expected screenshot bytes");
  const evidencePath = path.join(directory, "obs-current-evidence.json");
  valid.outputs[0] = {
    name: "source-screenshot.png",
    sha256: createHash("sha256").update(bytes).digest("hex"),
    size: bytes.length,
  };
  await writeFile(evidencePath, JSON.stringify(valid));
  await assert.rejects(
    loadObsEvidence(evidencePath, directory),
    /output.*missing/i,
  );
  await writeFile(
    path.join(directory, "source-screenshot.png"),
    Buffer.alloc(bytes.length),
  );
  await assert.rejects(
    loadObsEvidence(evidencePath, directory),
    /output.*(size|SHA-256)/i,
  );
});

test("release generation remains fail-closed before OBS evidence exists", async () => {
  const directory = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-manifest-"),
  );
  const manifest = await generateReleaseManifest({
    templatePath,
    evidenceDirectory: directory,
  });
  assert.equal(manifest.evidence.length, 0);
  assert.equal(manifest.scenarios.length, 6);
  assert.ok(
    manifest.scenarios.every((scenario) => scenario.evidence.length === 0),
  );
});

test("release generation binds strict committed browser evidence", async () => {
  const root = await mkdtemp(
    path.join(os.tmpdir(), "pepicanvas-browser-evidence-"),
  );
  const directory = path.join(root, "releases", "evidence");
  await mkdir(directory, { recursive: true });
  const localTemplate = path.join(root, "releases", "template.json");
  await writeFile(localTemplate, await readFile(templatePath));
  const evidencePath = path.join(directory, "chrome-current.json");
  await writeFile(
    evidencePath,
    JSON.stringify({
      schemaVersion: 1,
      slot: "chrome-current",
      browserVersion: "155.0.8059.79",
      releaseDate: "2026-10-13",
      scenarioId: "chrome-current",
      result: "PASS",
      startedAt: "2026-10-15T10:00:00Z",
      completedAt: "2026-10-15T10:01:00Z",
      correlationId: "browser-current-test",
      runtime: {
        os: "linux",
        architecture: "x64",
        node: "24.16.0",
        playwright: "1.64.0",
      },
      outputs: [{ name: "report.json", sha256: "a".repeat(64), size: 10 }],
    }),
  );
  const git = (...args) =>
    execFileSync("git", args, { cwd: root, encoding: "utf8" });
  git("init", "-q");
  const releaseOptions = {
    templatePath: localTemplate,
    evidenceDirectory: directory,
    repositoryUrl: "https://github.com/enzoruiz/pepicanvas",
    evidenceCommit: "a".repeat(40),
  };
  await assert.rejects(generateReleaseManifest(releaseOptions), /Git commit/i);
  git("add", "releases");
  git(
    "-c",
    "user.name=Test",
    "-c",
    "user.email=test@example.invalid",
    "commit",
    "-qm",
    "test fixture",
  );
  const commit = git("rev-parse", "HEAD").trim();
  releaseOptions.evidenceCommit = commit;
  await writeFile(evidencePath, `${await readFile(evidencePath, "utf8")}\n`);
  await assert.rejects(
    generateReleaseManifest(releaseOptions),
    /committed evidence.*match/i,
  );
  git("checkout", "--", "releases/evidence/chrome-current.json");
  const manifest = await generateReleaseManifest(releaseOptions);
  assert.equal(manifest.evidence.length, 1);
  assert.deepEqual(manifest.scenarios[0].evidence, ["evidence-chrome-current"]);
  assert.match(
    manifest.evidence[0].location,
    new RegExp(`blob/${commit}/releases/evidence`),
  );
});

test("static server injects the diagnostic into the real overlay and logs a valid snapshot", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "pepicanvas-static-test-"));
  const overlayDirectory = path.join(root, "overlay");
  await mkdir(overlayDirectory);
  const assetsDirectory = path.join(root, "assets");
  await mkdir(assetsDirectory);
  await writeFile(
    path.join(assetsDirectory, "app.js"),
    "export const real = true;\n",
  );
  const outsideName = `${path.basename(root)}-outside.txt`;
  await writeFile(path.join(root, "..", outsideName), "outside root\n");
  const overlayHtml =
    '<!doctype html><html><body><main id="real-overlay">Built overlay</main></body></html>';
  await writeFile(path.join(overlayDirectory, "index.html"), overlayHtml);
  const server = await startStaticServer(root);
  try {
    const response = await fetch(`${server.baseUrl}/overlay/`);
    const servedHtml = await response.text();
    assert.equal(response.status, 200);
    assert.match(servedHtml, /id="real-overlay">Built overlay/);
    assert.match(servedHtml, /navigator\.userAgentData/);
    assert.match(servedHtml, /window\.obsstudio/);
    assert.equal(
      servedHtml.replace(
        /<script id="pepicanvas-obs-diagnostic">[\s\S]*?<\/script>/,
        "",
      ),
      overlayHtml,
    );
    assert.equal(
      await (await fetch(`${server.baseUrl}/assets/app.js`)).text(),
      "export const real = true;\n",
    );
    assert.equal(
      (await fetch(`${server.baseUrl}/%2e%2e/${outsideName}`)).status,
      404,
    );

    const snapshot = {
      navigatorUserAgent: "OBS Browser/1.0",
      userAgentDataMobile: null,
      maxTouchPoints: 0,
      innerWidth: 1280,
      innerHeight: 720,
      obsIntegration: { exists: true, keys: ["getStatus", "pluginVersion"] },
    };
    const diagnosticReady = waitForDiagnostic(
      `${server.baseUrl}${diagnosticPath}`,
      server.child,
    );
    const report = await fetch(`${server.baseUrl}${diagnosticPath}`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "user-agent": "Harness Test/1.0",
      },
      body: JSON.stringify(snapshot),
    });
    assert.equal(report.status, 204);
    await diagnosticReady;
    assert.match(
      server.output(),
      new RegExp(
        `obs-client-snapshot ${JSON.stringify({ httpUserAgent: "Harness Test/1.0", ...snapshot }).replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}`,
      ),
    );
  } finally {
    await stopStaticServer(server.child);
  }
});

test("diagnostic endpoint rejects malformed and unexpected payloads", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "pepicanvas-static-test-"));
  const overlayDirectory = path.join(root, "overlay");
  await mkdir(overlayDirectory);
  await writeFile(path.join(overlayDirectory, "index.html"), "<body></body>");
  const server = await startStaticServer(root);
  try {
    const malformed = await fetch(`${server.baseUrl}${diagnosticPath}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: "{",
    });
    assert.equal(malformed.status, 400);

    const unexpected = await fetch(`${server.baseUrl}${diagnosticPath}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ unexpected: true }),
    });
    assert.equal(unexpected.status, 400);
    assert.doesNotMatch(server.output(), /obs-client-snapshot \{/);
  } finally {
    await stopStaticServer(server.child);
  }
});

test("diagnostic endpoint rejects oversized request bodies", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "pepicanvas-static-test-"));
  const overlayDirectory = path.join(root, "overlay");
  await mkdir(overlayDirectory);
  await writeFile(path.join(overlayDirectory, "index.html"), "<body></body>");
  const server = await startStaticServer(root);
  try {
    const response = await fetch(`${server.baseUrl}${diagnosticPath}`, {
      method: "POST",
      headers: { "content-type": "application/json" },
      body: JSON.stringify({ navigatorUserAgent: "x".repeat(5_000) }),
    });
    assert.equal(response.status, 413);
    assert.doesNotMatch(server.output(), /obs-client-snapshot \{/);
  } finally {
    await stopStaticServer(server.child);
  }
});

test("Linux reports Windows execution as pending without launching OBS", async () => {
  if (process.platform === "win32") return;
  await assert.rejects(
    executeObsSlot({
      slot: "obs-current",
      offline: true,
      outputRoot: os.tmpdir(),
    }),
    /Windows OBS execution is pending/i,
  );
});

test("workflow is least-privilege, bounded, and SHA-pinned", async () => {
  const workflow = await readFile(
    path.join(repositoryRoot, ".github/workflows/client-support-release.yml"),
    "utf8",
  );
  assert.match(workflow, /workflow_dispatch:/);
  assert.match(workflow, /pull_request:/);
  assert.match(workflow, /permissions:\r?\n  contents: read/);
  assert.match(workflow, /runs-on: windows-latest/);
  assert.match(workflow, /timeout-minutes: 45/);
  assert.doesNotMatch(workflow, /secrets\./);
  const uses = [...workflow.matchAll(/uses: ([^\s]+)/g)].map(
    (match) => match[1],
  );
  assert.ok(uses.length >= 4);
  assert.ok(uses.every((value) => /@[0-9a-f]{40}$/.test(value)));
});
