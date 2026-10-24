/* global process, setTimeout, window */

// TE-010 remote render latency: one transmission per browser of the matrix.
// Node submits each command to the harness server, the probe page renders
// the event it receives over SSE, and the page clock is mapped onto the
// server clock with pings around every sample. The runner validates the
// evidence written here with backend/cmd/latencycheck.

import { randomBytes } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import path from "node:path";

import { expect, test } from "@playwright/test";

import { loadBrowserMatrix } from "../../scripts/browser-matrix.mjs";
import {
  assembleEvidence,
  commandPlan,
  scenarioId,
  transmissionId,
} from "../../scripts/latency-evidence.mjs";

const repositoryRoot = path.resolve(import.meta.dirname, "../../..");
const cycles = 20;
const renderTimeoutMs = 10_000;

function requiredEnv(name) {
  const value = process.env[name];
  if (!value) {
    throw new Error(`${name} is required; run npm run latency:probe`);
  }
  return value;
}

async function serverNow(request) {
  const response = await request.get("/probe/ping");
  expect(response.status()).toBe(200);
  return (await response.json()).serverAtNs;
}

async function submit(request, step) {
  const operationId = `op-${randomBytes(12).toString("hex")}`;
  const data = { operationId, objectId: step.objectId, kind: step.command };
  if (step.payload !== undefined) {
    data.payload = step.payload;
  }
  const response = await request.post("/probe/commands", { data });
  expect(response.status()).toBe(200);
  const body = await response.json();
  expect(body.outcome).toBe("accepted");
  return { operationId, ...body };
}

function pings(page, n) {
  return page.evaluate((count) => window.__pepicanvasLatency.pings(count), n);
}

function disconnects(page) {
  return page.evaluate(() => window.__pepicanvasLatency.disconnects());
}

function rendered(page, operationId) {
  return page.evaluate(
    ({ id, timeoutMs }) =>
      Promise.race([
        window.__pepicanvasLatency.rendered(id),
        new Promise((_, reject) =>
          setTimeout(
            () => reject(new Error(`no render for ${id} in ${timeoutMs} ms`)),
            timeoutMs,
          ),
        ),
      ]),
    { id: operationId, timeoutMs: renderTimeoutMs },
  );
}

test("measures remote render latency from server acceptance", async ({
  browser,
  page,
  request,
}, testInfo) => {
  const evidenceDir = requiredEnv("PEPICANVAS_LATENCY_EVIDENCE_DIR");
  const versions = JSON.parse(
    await readFile(requiredEnv("PEPICANVAS_LATENCY_VERSIONS"), "utf8"),
  );
  const matrix = await loadBrowserMatrix(
    path.join(repositoryRoot, "releases", "client-browser-matrix.json"),
  );
  const slot = testInfo.project.name;
  const entry = matrix.browsers.find((candidate) => candidate.slot === slot);
  expect(entry).toBeDefined();

  const started = new Date();
  await page.goto("/latency/");
  await page.evaluate(() => window.__pepicanvasLatency.ready());
  const measurementStartNs = await serverNow(request);

  const samples = [];
  for (const [index, step] of commandPlan(cycles).entries()) {
    const before = await disconnects(page);
    const leading = await pings(page, 3);
    const command = await submit(request, step);
    const mark = await rendered(page, command.operationId);
    const trailing = await pings(page, 2);
    const after = await disconnects(page);
    samples.push({
      id: `s-${String(index + 1).padStart(3, "0")}`,
      kind: step.kind,
      operationId: command.operationId,
      sequence: command.sequence,
      commandCorrelationId: command.correlationId,
      renderCorrelationId: mark.correlationId,
      acceptedAtNs: command.acceptedAtNs,
      renderedAtMs: mark.renderedAtMs,
      pings: [...leading, ...trailing],
      disconnects: after - before,
    });
  }
  const completed = new Date();

  const evidence = assembleEvidence({
    transmission: {
      id: transmissionId(slot, started),
      scenarioId,
      correlationId: randomBytes(16).toString("hex"),
      startedAt: started.toISOString(),
      completedAt: completed.toISOString(),
      measurementStartNs,
      client: {
        family: entry.family,
        slot: entry.releaseTrack,
        version: browser.version(),
      },
    },
    versions,
    samples,
  });
  await writeFile(
    path.join(evidenceDir, `${slot}.json`),
    `${JSON.stringify(evidence, null, 2)}\n`,
  );
});
