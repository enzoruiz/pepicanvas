/* global Buffer, fetch, process */

// US-016 end to end: an activated owner sees the storage usage of the own
// canvas in the editor, over the real server and PostgreSQL. The e2e server
// runs without object storage, so an upload is answered media_unavailable
// and must not change the usage; uploads that succeed are covered by the
// backend HTTP and MinIO tests.

import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";

import { expect, test } from "@playwright/test";

const password = `e2e-password-${randomBytes(6).toString("hex")}`;
const backendRoot = path.resolve(import.meta.dirname, "../../../backend");

const emptyUsage = {
  quota_bytes: 1_000_000_000,
  used_bytes: 0,
  reserved_bytes: 0,
  available_bytes: 1_000_000_000,
};

function uniqueEmail(label) {
  return `${label}-${randomBytes(6).toString("hex")}@example.com`;
}

// Grants the operator role from the host, as an administrator would.
function grantOperator(email) {
  const result = spawnSync(
    "go",
    ["run", "./cmd/server", "grant-operator", email],
    {
      cwd: backendRoot,
      encoding: "utf8",
      env: {
        ...process.env,
        PEPICANVAS_DATABASE_URL: process.env.PEPICANVAS_E2E_DATABASE_URL,
      },
    },
  );
  expect(result.status, result.stderr).toBe(0);
}

async function openEditor(page) {
  await page.goto("/editor/");
  await expect(
    page.getByRole("heading", { level: 1, name: "PepiCanvas editor" }),
  ).toBeVisible();
}

async function register(page, email) {
  await page.getByRole("button", { name: "Create an account" }).click();
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("status")).toHaveText(
    "Account created. You can sign in now.",
  );
}

// Signs in an account in its own browser context, so sessions stay apart.
async function signedIn(browser, email) {
  const context = await browser.newContext();
  const page = await context.newPage();
  await openEditor(page);
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText(`Signed in as ${email}`)).toBeVisible();
  return { context, page };
}

async function activate(page, email) {
  const panel = page.getByRole("region", { name: "Activate a streamer" });
  await panel.getByLabel("Streamer email").fill(email);
  await panel.getByRole("button", { name: "Activate" }).click();
  await expect(panel.getByRole("status")).toHaveText("Streamer activated.");
}

function storagePanel(page) {
  return page.getByRole("region", { name: "Canvas storage" });
}

async function ownCanvas(page) {
  const response = await page.request.get("/api/canvas");
  expect(response.status()).toBe(200);
  return response.json();
}

async function usageOf(page, canvas) {
  return page.request.get(
    `/api/tenants/${canvas.tenant_id}/canvases/${canvas.canvas_id}/media/usage`,
  );
}

async function expectEmptyStorage(page) {
  const panel = storagePanel(page);
  await expect(
    panel.getByText("Used 0 of 1,000,000,000 bytes", { exact: true }),
  ).toBeVisible();
  await expect(
    panel.getByText("Available: 1,000,000,000 bytes", { exact: true }),
  ).toBeVisible();
  await expect(panel.getByText(/Reserved/)).toHaveCount(0);
  await expect(
    panel.getByRole("meter", { name: "Storage used" }),
  ).toHaveAttribute("value", "0");
}

test("an owner sees the canvas storage and an upload without object storage changes nothing", async ({
  browser,
  page,
}) => {
  const operator = uniqueEmail("olga");
  const alice = uniqueEmail("alice");
  const bob = uniqueEmail("bob");

  await openEditor(page);
  for (const email of [operator, alice, bob]) {
    await register(page, email);
  }
  grantOperator(operator);
  const op = await signedIn(browser, operator);
  await activate(op.page, alice);
  await activate(op.page, bob);
  // The operator owns no canvas, so it never sees the storage panel.
  await expect(storagePanel(op.page)).toHaveCount(0);

  // S1 in the editor: the exact decimal quota of an empty canvas.
  const a = await signedIn(browser, alice);
  await expectEmptyStorage(a.page);
  const aliceCanvas = await ownCanvas(a.page);
  const before = await usageOf(a.page, aliceCanvas);
  expect(before.status()).toBe(200);
  expect(await before.json()).toEqual(emptyUsage);

  // S17 without object storage: the upload is refused with the fixed
  // message, and nothing is reserved or consumed.
  const uploads = [];
  a.page.on("response", (response) => {
    if (response.url().includes("/media/uploads?")) {
      uploads.push(response.status());
    }
  });
  const form = storagePanel(a.page).getByRole("form", {
    name: "Upload a file",
  });
  await form.getByLabel("File").setInputFiles({
    name: "pixel.png",
    mimeType: "image/png",
    buffer: Buffer.from("not really a png"),
  });
  await form.getByRole("button", { name: "Upload" }).click();
  await expect(storagePanel(a.page).getByRole("alert")).toHaveText(
    "Uploads are not available on this server.",
  );
  expect(uploads).toEqual([503]);
  await expectEmptyStorage(a.page);
  expect(await (await usageOf(a.page, aliceCanvas)).json()).toEqual(emptyUsage);
  await a.page.reload();
  await expectEmptyStorage(a.page);

  // S8 isolation: B, with a valid session, cannot read A's usage, and the
  // denial carries no usage figures.
  const b = await signedIn(browser, bob);
  await expectEmptyStorage(b.page);
  const cross = await b.page.evaluate(
    async ({ tenantId, canvasId }) => {
      const response = await fetch(
        `/api/tenants/${tenantId}/canvases/${canvasId}/media/usage`,
        { credentials: "same-origin" },
      );
      return { status: response.status, body: await response.json() };
    },
    { tenantId: aliceCanvas.tenant_id, canvasId: aliceCanvas.canvas_id },
  );
  expect(cross.status).toBe(403);
  expect(cross.body.error).toBe("forbidden");
  for (const key of Object.keys(emptyUsage)) {
    expect(cross.body).not.toHaveProperty(key);
  }

  for (const session of [op, a, b]) {
    await session.context.close();
  }
});
