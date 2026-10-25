import { readdir, readFile } from "node:fs/promises";
import path from "node:path";

const canonicalCorrelation = /^[0-9a-f]{32}$/;

// decoyNeedles lists every string whose presence in evidence is a leak: each
// decoy value, its URL-encoded form, and the signature of the media URL on
// its own, since scrubbing a URL must remove the signature too.
export function decoyNeedles(decoys) {
  const needles = [];
  for (const [label, value] of Object.entries(decoys)) {
    needles.push({ label, values: unique([value, encodeURIComponent(value)]) });
  }
  if (decoys.media_url) {
    const signature = new URL(decoys.media_url).searchParams.get(
      "X-Amz-Signature",
    );
    if (signature) {
      needles.push({ label: "media_url signature", values: [signature] });
    }
  }
  return needles;
}

function unique(values) {
  return [...new Set(values)];
}

async function listFiles(entry) {
  let entries;
  try {
    entries = await readdir(entry, { withFileTypes: true });
  } catch (error) {
    if (error.code === "ENOENT") return [];
    if (error.code === "ENOTDIR") return [entry];
    throw error;
  }
  const files = [];
  for (const child of entries) {
    const childPath = path.join(entry, child.name);
    files.push(
      ...(child.isDirectory() ? await listFiles(childPath) : [childPath]),
    );
  }
  return files;
}

// scanFiles reports every line of every file under paths that contains a
// needle. Missing paths are skipped so an empty Playwright output directory is
// not an error.
export async function scanFiles(paths, needles) {
  const files = (await Promise.all(paths.map(listFiles))).flat().sort();
  const findings = [];
  for (const file of files) {
    const lines = (await readFile(file, "utf8")).split("\n");
    for (const { label, values } of needles) {
      lines.forEach((text, index) => {
        if (values.some((value) => value && text.includes(value))) {
          findings.push({ file, label, line: index + 1 });
        }
      });
    }
  }
  return findings;
}

// checkRotation reports every decoy value of the current slot that an
// earlier slot already used.
export function checkRotation(earlier, decoys) {
  const findings = [];
  for (const [label, value] of Object.entries(decoys)) {
    const owner = earlier.find((slot) =>
      Object.values(slot.decoys).includes(value),
    );
    if (owner) {
      findings.push({
        rule: "rotation",
        detail: `${label} repeats a decoy from ${owner.slot}`,
      });
    }
  }
  return findings;
}

// checkCorrelation requires a canonical correlation_id on every JSON log
// record except the named process-scoped messages, so a new request-scoped
// record is checked by default.
export function checkCorrelation(log, { processMessages }) {
  const findings = [];
  let requestRecords = 0;
  log.split("\n").forEach((text, index) => {
    if (text.trim() === "") return;
    const line = index + 1;
    let record;
    try {
      record = JSON.parse(text);
    } catch {
      findings.push({
        rule: "correlation",
        line,
        detail: "record is not valid JSON",
      });
      return;
    }
    if (processMessages.includes(record.msg)) return;
    requestRecords += 1;
    if (!canonicalCorrelation.test(record.correlation_id ?? "")) {
      findings.push({
        rule: "correlation",
        line,
        detail: `record "${record.msg}" lacks a canonical correlation_id`,
      });
    }
  });
  if (requestRecords === 0 && findings.length === 0) {
    findings.push({
      rule: "correlation",
      line: 0,
      detail: "no request records were logged",
    });
  }
  return findings;
}

// evaluateSlot applies every evidence rule to one slot: no decoy from this or
// any earlier slot appears under evidenceRoot, the decoys rotated, and every
// request record in evidenceRoot/fixture.log is correlated.
export async function evaluateSlot({
  slot,
  decoys,
  earlier,
  evidenceRoot,
  processMessages,
}) {
  const findings = checkRotation(earlier, decoys);
  for (const owner of [...earlier, { slot, decoys }]) {
    for (const { file, label, line } of await scanFiles(
      [evidenceRoot],
      decoyNeedles(owner.decoys),
    )) {
      findings.push({
        rule: "decoy",
        detail: `${owner.slot} ${label} found in ${path.relative(evidenceRoot, file)}:${line}`,
      });
    }
  }
  let log = "";
  try {
    log = await readFile(path.join(evidenceRoot, "fixture.log"), "utf8");
  } catch (error) {
    if (error.code !== "ENOENT") throw error;
  }
  for (const finding of checkCorrelation(log, { processMessages })) {
    findings.push({
      rule: finding.rule,
      detail: `fixture.log:${finding.line} ${finding.detail}`,
    });
  }
  return findings;
}
