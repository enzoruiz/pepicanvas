import { createReadStream } from "node:fs";
import { stat } from "node:fs/promises";
import http from "node:http";
import path from "node:path";

const [rootArgument, portArgument] = process.argv.slice(2);
if (!rootArgument || !portArgument || !/^\d+$/.test(portArgument)) {
  throw new Error("usage: node static-server.mjs <root> <port>");
}
const root = path.resolve(rootArgument);
const port = Number(portArgument);
const contentTypes = new Map([
  [".css", "text/css; charset=utf-8"],
  [".html", "text/html; charset=utf-8"],
  [".js", "text/javascript; charset=utf-8"],
  [".svg", "image/svg+xml"],
]);

const server = http.createServer(async (request, response) => {
  try {
    const pathname = new URL(request.url, `http://${request.headers.host}`)
      .pathname;
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
    response.writeHead(200, {
      "cache-control": "no-store",
      "content-type":
        contentTypes.get(path.extname(file)) ?? "application/octet-stream",
    });
    createReadStream(file).pipe(response);
  } catch {
    response.writeHead(404).end("Not found");
  }
});

server.listen(port, "127.0.0.1", () => {
  console.log(`serving ${root} at http://127.0.0.1:${port}`);
});
