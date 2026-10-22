/* global console, process, setTimeout */

// Runs the US-001, US-002, and US-007 end-to-end scenarios on every browser
// of the support matrix against a disposable PostgreSQL container and the
// real Go server, which serves the production frontend build. The scenarios
// get the database URL in PEPICANVAS_E2E_DATABASE_URL, which the server and
// the host-side `server grant-operator` command share. Each spec file runs
// in its own Playwright invocation, so it gets a fresh server whose per-IP
// rate limits are not spent by the other files.

import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import { readdir, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import {
  prepareBrowserCache,
  prepareExtractedBrowser,
} from "./browser-cache.mjs";
import { browserCachePaths, repositoryRoot } from "./browser-matrix.mjs";

// Matches backend/internal/platform/postgres/pgtest.Image (CTRL-VERS-01).
const postgresImage =
  "postgres:17.4-alpine@sha256:7062a2109c4b51f3c792c7ea01e83ed12ef9a980886e3b3d380a7d2e5f6ce3f5";
const frontendRoot = path.join(repositoryRoot, "frontend");
const specRoot = path.join(frontendRoot, "tests/browser");

async function specFiles() {
  const files = (await readdir(specRoot))
    .filter((name) => name.endsWith(".auth.mjs"))
    .sort();
  if (files.length === 0) {
    throw new Error("no *.auth.mjs scenarios found");
  }
  return files.map((name) => path.posix.join("tests/browser", name));
}

function docker(args) {
  const result = spawnSync("docker", args, { encoding: "utf8" });
  if (result.error || result.status !== 0) {
    const detail = result.stderr?.trim() || result.error?.message || "failed";
    throw new Error(`docker ${args[0]}: ${detail}`);
  }
  return result.stdout.trim();
}

async function startPostgres() {
  const password = randomBytes(16).toString("hex");
  const id = docker([
    "run",
    "--detach",
    "--rm",
    "--publish",
    "127.0.0.1::5432",
    "--env",
    "POSTGRES_USER=pepicanvas",
    "--env",
    "POSTGRES_DB=pepicanvas",
    "--env",
    `POSTGRES_PASSWORD=${password}`,
    postgresImage,
  ]);
  const port = docker(["port", id, "5432/tcp"]).split("\n")[0].split(":").pop();
  // The image's init server listens only on its socket; TCP readiness means
  // the final server is up.
  for (let attempt = 0; attempt < 120; attempt++) {
    const ready = spawnSync("docker", [
      "exec",
      id,
      "pg_isready",
      "--host",
      "127.0.0.1",
      "--username",
      "pepicanvas",
    ]);
    if (ready.status === 0) {
      return {
        id,
        url: `postgres://pepicanvas:${password}@127.0.0.1:${port}/pepicanvas?sslmode=disable`,
      };
    }
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  docker(["rm", "--force", id]);
  throw new Error("PostgreSQL did not become ready");
}

async function main() {
  const [mode, ...rest] = process.argv.slice(2);
  if (rest.length > 0 || !["--download", "--offline"].includes(mode)) {
    throw new Error(
      "usage: node scripts/auth-e2e-runner.mjs --download|--offline",
    );
  }
  const specs = await specFiles();
  const matrix = await prepareBrowserCache({ offline: mode === "--offline" });
  const build = spawnSync("npm", ["run", "build"], {
    cwd: frontendRoot,
    stdio: "inherit",
  });
  if (build.error || build.status !== 0) {
    throw new Error("frontend build failed");
  }
  const database = await startPostgres();
  const failed = [];
  try {
    for (const browser of matrix.browsers) {
      const paths = browserCachePaths(frontendRoot, browser);
      try {
        const { versionOutput } = await prepareExtractedBrowser(browser);
        console.log(`${browser.slot}: executing ${versionOutput}`);
        let browserFailed = false;
        for (const spec of specs) {
          const result = spawnSync(
            process.execPath,
            [
              path.join(frontendRoot, "node_modules/@playwright/test/cli.js"),
              "test",
              "--config",
              "playwright.auth.config.ts",
              // The = form: a bare --project would take the spec as a
              // second project name.
              `--project=${browser.slot}`,
              spec,
            ],
            {
              cwd: frontendRoot,
              env: {
                ...process.env,
                PEPICANVAS_E2E_DATABASE_URL: database.url,
              },
              stdio: "inherit",
            },
          );
          if (result.error || result.status !== 0) {
            browserFailed = true;
          }
        }
        if (browserFailed) {
          failed.push(browser.slot);
        }
      } finally {
        await rm(paths.extractionPath, { force: true, recursive: true });
      }
    }
  } finally {
    docker(["rm", "--force", database.id]);
  }
  if (failed.length > 0) {
    throw new Error(
      `account, session, and activation scenarios failed on ${failed.join(", ")}`,
    );
  }
  console.log(
    `account, session, and activation scenarios passed on ${matrix.browsers.length} browsers`,
  );
}

if (process.argv[1] === fileURLToPath(import.meta.url)) {
  main().catch((error) => {
    console.error(error.message);
    process.exitCode = 1;
  });
}
