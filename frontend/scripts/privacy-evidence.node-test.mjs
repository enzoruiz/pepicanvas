import assert from "node:assert/strict";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  checkCorrelation,
  checkRotation,
  decoyNeedles,
  evaluateSlot,
  scanFiles,
} from "./privacy-evidence.mjs";

const correlationA = "0123456789abcdef0123456789abcdef";
const decoysA = {
  invitation: "pci_AAAAinvitation",
  session: "pcs_AAAAsession0000",
  obs_credential: "pco_AAAAobs",
  media_url: "https://storage.invalid/m/1.png?X-Amz-Signature=sigAAAA",
  private_marker: "private-marker-AAAA",
};
const decoysB = {
  invitation: "pci_BBBBinvitation",
  session: "pcs_BBBBsession0000",
  obs_credential: "pco_BBBBobs",
  media_url: "https://storage.invalid/m/1.png?X-Amz-Signature=sigBBBB",
  private_marker: "private-marker-BBBB",
};

async function withEvidence(files, run) {
  const directory = await mkdtemp(path.join(os.tmpdir(), "privacy-evidence-"));
  try {
    for (const [name, content] of Object.entries(files)) {
      const target = path.join(directory, name);
      await mkdir(path.dirname(target), { recursive: true });
      await writeFile(target, content);
    }
    await run(directory);
  } finally {
    await rm(directory, { force: true, recursive: true });
  }
}

function logLine(record) {
  return `${JSON.stringify(record)}\n`;
}

test("a decoy in any collected file is reported with its file, kind, and line", async () => {
  await withEvidence(
    {
      "fixture.log": `${logLine({ msg: "ok" })}${logLine({ msg: `leak ${decoysA.session}` })}`,
      "test-results/case/error-context.md": `invite=${encodeURIComponent(decoysA.media_url)}`,
    },
    async (directory) => {
      const findings = await scanFiles([directory], decoyNeedles(decoysA));

      assert.deepEqual(
        findings.map(({ file, label, line }) => ({
          file: path.relative(directory, file),
          label,
          line,
        })),
        [
          { file: "fixture.log", label: "session", line: 2 },
          {
            file: path.join("test-results/case/error-context.md"),
            label: "media_url",
            line: 1,
          },
          {
            file: path.join("test-results/case/error-context.md"),
            label: "media_url signature",
            line: 1,
          },
        ],
      );
    },
  );
});

test("clean evidence and missing directories produce no findings", async () => {
  await withEvidence(
    {
      "fixture.log": logLine({
        msg: "request",
        url: "/private/canvas?[REDACTED]",
        correlation_id: correlationA,
      }),
      "playwright-output.txt": "6 passed",
    },
    async (directory) => {
      const findings = await scanFiles(
        [directory, path.join(directory, "absent-test-results")],
        decoyNeedles(decoysA),
      );
      assert.deepEqual(findings, []);
    },
  );
});

test("a slot that reuses any earlier decoy value is a rotation violation", () => {
  const reused = { ...decoysB, obs_credential: decoysA.obs_credential };

  assert.deepEqual(checkRotation([], decoysA), []);
  assert.deepEqual(
    checkRotation([{ slot: "chrome-current", decoys: decoysA }], decoysB),
    [],
  );
  assert.deepEqual(
    checkRotation([{ slot: "chrome-current", decoys: decoysA }], reused),
    [
      {
        rule: "rotation",
        detail: "obs_credential repeats a decoy from chrome-current",
      },
    ],
  );
});

test("request records without a canonical correlation_id are reported", () => {
  const log = [
    logLine({ msg: "privacy fixture listening", addr: "127.0.0.1:4174" }),
    logLine({ msg: "request", url: "/", correlation_id: correlationA }),
    logLine({ msg: "private page served" }),
    logLine({ msg: "request failed", correlation_id: "ABC" }),
    "not json\n",
  ].join("");

  assert.deepEqual(
    checkCorrelation(log, { processMessages: ["privacy fixture listening"] }),
    [
      {
        rule: "correlation",
        line: 3,
        detail: 'record "private page served" lacks a canonical correlation_id',
      },
      {
        rule: "correlation",
        line: 4,
        detail: 'record "request failed" lacks a canonical correlation_id',
      },
      { rule: "correlation", line: 5, detail: "record is not valid JSON" },
    ],
  );
  assert.deepEqual(checkCorrelation("", { processMessages: [] }), [
    { rule: "correlation", line: 0, detail: "no request records were logged" },
  ]);
});

test("an earlier slot's decoy appearing in later evidence fails the later slot", async () => {
  await withEvidence(
    {
      "fixture.log": logLine({
        msg: `stale ${decoysA.invitation}`,
        correlation_id: correlationA,
      }),
    },
    async (directory) => {
      const result = await evaluateSlot({
        slot: "chrome-previous",
        decoys: decoysB,
        earlier: [{ slot: "chrome-current", decoys: decoysA }],
        evidenceRoot: directory,
        processMessages: [],
      });

      assert.deepEqual(
        result.map(({ rule, detail }) => ({ rule, detail })),
        [
          {
            rule: "decoy",
            detail: "chrome-current invitation found in fixture.log:1",
          },
        ],
      );
    },
  );
});
