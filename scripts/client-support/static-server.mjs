import { createReadStream } from "node:fs";
import { readFile, stat } from "node:fs/promises";
import http from "node:http";
import path from "node:path";

const [rootArgument, portArgument] = process.argv.slice(2);
if (!rootArgument || !portArgument || !/^\d+$/.test(portArgument)) {
  throw new Error("usage: node static-server.mjs <root> <port>");
}
const root = path.resolve(rootArgument);
const port = Number(portArgument);
const diagnosticPath = "/__pepicanvas/obs-client-snapshot";
const maximumDiagnosticBytes = 4_096;
const diagnosticScript = `<script id="pepicanvas-obs-diagnostic">
(() => {
  const integration = window.obsstudio;
  const integrationExists =
    integration !== null &&
    (typeof integration === "object" || typeof integration === "function");
  const integrationKeys = integrationExists
    ? Object.keys(integration)
        .filter((key) => key.length <= 128)
        .sort()
        .slice(0, 32)
    : [];
  const finiteNumber = (value) => (Number.isFinite(value) ? value : 0);
  void fetch("${diagnosticPath}", {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      navigatorUserAgent: navigator.userAgent.slice(0, 512),
      userAgentDataMobile:
        typeof navigator.userAgentData?.mobile === "boolean"
          ? navigator.userAgentData.mobile
          : null,
      maxTouchPoints: finiteNumber(navigator.maxTouchPoints),
      innerWidth: finiteNumber(window.innerWidth),
      innerHeight: finiteNumber(window.innerHeight),
      obsIntegration: {
        exists: integrationExists,
        keys: integrationKeys,
      },
    }),
    keepalive: true,
  }).catch(() => {});
})();
</script>`;
const contentTypes = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".svg", "image/svg+xml"],
]);
let diagnosticReceived = false;

function hasExactKeys(value, expected) {
  return (
    value !== null &&
    typeof value === "object" &&
    !Array.isArray(value) &&
    Object.keys(value).sort().join("\0") === [...expected].sort().join("\0")
  );
}

function isBoundedFiniteNumber(value, maximum) {
  return (
    typeof value === "number" &&
    Number.isFinite(value) &&
    value >= 0 &&
    value <= maximum
  );
}

function validateSnapshot(value) {
  const expected = [
    "navigatorUserAgent",
    "userAgentDataMobile",
    "maxTouchPoints",
    "innerWidth",
    "innerHeight",
    "obsIntegration",
  ];
  if (!hasExactKeys(value, expected)) return false;
  if (
    typeof value.navigatorUserAgent !== "string" ||
    value.navigatorUserAgent.length > 512 ||
    /[\u0000-\u001f\u007f]/.test(value.navigatorUserAgent) ||
    ![true, false, null].includes(value.userAgentDataMobile) ||
    !isBoundedFiniteNumber(value.maxTouchPoints, 1_000) ||
    !isBoundedFiniteNumber(value.innerWidth, 1_000_000) ||
    !isBoundedFiniteNumber(value.innerHeight, 1_000_000) ||
    !hasExactKeys(value.obsIntegration, ["exists", "keys"]) ||
    typeof value.obsIntegration.exists !== "boolean" ||
    !Array.isArray(value.obsIntegration.keys) ||
    value.obsIntegration.keys.length > 32
  ) {
    return false;
  }
  const keys = value.obsIntegration.keys;
  if (
    keys.some(
      (key) =>
        typeof key !== "string" ||
        key.length > 128 ||
        /[\u0000-\u001f\u007f]/.test(key),
    ) ||
    keys.some((key, index) => index > 0 && key <= keys[index - 1]) ||
    (!value.obsIntegration.exists && keys.length > 0)
  ) {
    return false;
  }
  return true;
}

function sanitizeUserAgent(value) {
  return String(value ?? "")
    .replace(/[\u0000-\u001f\u007f]/g, "")
    .slice(0, 512);
}

async function readRequestBody(request) {
  const declaredLength = Number(request.headers["content-length"]);
  if (
    Number.isFinite(declaredLength) &&
    declaredLength > maximumDiagnosticBytes
  ) {
    request.resume();
    return { oversized: true };
  }
  const chunks = [];
  let size = 0;
  let oversized = false;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > maximumDiagnosticBytes) {
      oversized = true;
      continue;
    }
    chunks.push(chunk);
  }
  return oversized
    ? { oversized: true }
    : { oversized: false, body: Buffer.concat(chunks).toString("utf8") };
}

async function handleDiagnostic(request, response) {
  if (request.method === "GET") {
    response.writeHead(diagnosticReceived ? 200 : 202).end();
    return;
  }
  if (request.method !== "POST") {
    response.writeHead(405, { allow: "GET, POST" }).end();
    return;
  }
  if (request.headers["content-type"] !== "application/json") {
    response.writeHead(415).end();
    return;
  }
  const payload = await readRequestBody(request);
  if (payload.oversized) {
    response.writeHead(413).end();
    return;
  }
  let snapshot;
  try {
    snapshot = JSON.parse(payload.body);
  } catch {
    response.writeHead(400).end();
    return;
  }
  if (!validateSnapshot(snapshot)) {
    response.writeHead(400).end();
    return;
  }
  if (!diagnosticReceived) {
    diagnosticReceived = true;
    console.log(
      `obs-client-snapshot ${JSON.stringify({
        httpUserAgent: sanitizeUserAgent(request.headers["user-agent"]),
        ...snapshot,
      })}`,
    );
  }
  response.writeHead(204).end();
}

const server = http.createServer(async (request, response) => {
  try {
    const pathname = new URL(request.url, `http://${request.headers.host}`)
      .pathname;
    if (pathname === diagnosticPath) {
      await handleDiagnostic(request, response);
      return;
    }
    const requested = pathname.endsWith("/")
      ? `${pathname}index.html`
      : pathname;
    const file = path.resolve(root, `.${requested}`);
    const relative = path.relative(root, file);
    if (
      relative.startsWith("..") ||
      path.isAbsolute(relative) ||
      !(await stat(file)).isFile()
    ) {
      response.writeHead(404).end("Not found");
      return;
    }
    const headers = {
      "cache-control": "no-store",
      "content-type":
        contentTypes.get(path.extname(file)) ?? "application/octet-stream",
    };
    if (
      (pathname === "/overlay/" || pathname === "/overlay/index.html") &&
      path.extname(file) === ".html"
    ) {
      const html = await readFile(file, "utf8");
      const instrumented = /<\/body>/i.test(html)
        ? html.replace(/<\/body>/i, `${diagnosticScript}$&`)
        : `${html}${diagnosticScript}`;
      response.writeHead(200, headers).end(instrumented);
      return;
    }
    response.writeHead(200, headers);
    createReadStream(file).pipe(response);
  } catch {
    response.writeHead(404).end("Not found");
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`serving ${root} at http://127.0.0.1:${port}`);
});
