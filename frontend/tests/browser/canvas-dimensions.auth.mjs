// US-007 end to end: the owner resizes the canvas from the editor form over
// the real server and PostgreSQL. The private OBS URL is US-021, so the
// overlay side of "OBS and editor show the same confirmed size" is not
// covered here; the editor surface is compared with GET /api/canvas.

import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";

import { expect, test } from "@playwright/test";

const password = `e2e-password-${randomBytes(6).toString("hex")}`;
const backendRoot = path.resolve(import.meta.dirname, "../../../backend");

const saved = "Canvas size saved.";
const widthMessage = "Width must be a whole number from 320 to 3840.";

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
  const panel = page.getByRole("region", { name: "Manage streamers" });
  await panel.getByLabel("Streamer email").fill(email);
  await panel.getByRole("button", { name: "Activate", exact: true }).click();
  await expect(panel.getByRole("status")).toHaveText("Streamer activated.");
}

function canvasRegion(page) {
  return page.getByRole("region", { name: "Your canvas" });
}

function sizeForm(page) {
  return canvasRegion(page).getByRole("form", { name: "Canvas size" });
}

async function ownCanvas(page) {
  const response = await page.request.get("/api/canvas");
  expect(response.status()).toBe(200);
  return response.json();
}

// The surface's rendered size in CSS pixels, unclipped by its scroll box.
async function surfaceSize(page) {
  const surface = canvasRegion(page).locator("[data-canvas-surface]");
  await expect(surface).toHaveCount(1);
  return surface.evaluate((element) => {
    const { width, height } = element.getBoundingClientRect();
    return { width, height };
  });
}

// The editor shows the confirmed size in the label, the form, and the
// surface, and it matches what the server reports.
async function expectConfirmed(page, width, height) {
  await expect(
    canvasRegion(page).getByText(`${width} × ${height}`),
  ).toBeVisible();
  await expect(sizeForm(page).getByLabel("Width")).toHaveValue(String(width));
  await expect(sizeForm(page).getByLabel("Height")).toHaveValue(String(height));
  expect(await surfaceSize(page)).toEqual({ width, height });
  expect(await ownCanvas(page)).toMatchObject({ width, height });
}

async function resize(page, width, height) {
  await sizeForm(page).getByLabel("Width").fill(String(width));
  await sizeForm(page).getByLabel("Height").fill(String(height));
  await sizeForm(page).getByRole("button", { name: "Apply size" }).click();
}

test("the owner resizes the canvas within the limits and only the owner can", async ({
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

  // S1 initial value.
  const a = await signedIn(browser, alice);
  await expectConfirmed(a.page, 1920, 1080);

  // S1 inclusive limits, persisted by the server.
  await resize(a.page, 320, 180);
  await expect(canvasRegion(a.page).getByRole("status")).toHaveText(saved);
  await expectConfirmed(a.page, 320, 180);
  await resize(a.page, 3840, 2160);
  await expect(canvasRegion(a.page).getByRole("status")).toHaveText(saved);
  await expectConfirmed(a.page, 3840, 2160);

  // S2 in the editor: a fractional width is rejected before any request.
  const puts = [];
  a.page.on("request", (request) => {
    if (request.method() === "PUT" && request.url().endsWith("/dimensions")) {
      puts.push(request.url());
    }
  });
  await resize(a.page, "1.5", 1080);
  await expect(canvasRegion(a.page).getByRole("alert")).toHaveText(
    widthMessage,
  );
  expect(puts).toEqual([]);
  await expect(canvasRegion(a.page).getByText("3840 × 2160")).toBeVisible();
  expect(await ownCanvas(a.page)).toMatchObject({ width: 3840, height: 2160 });

  // S6 reconnection: typed but unsaved values are gone after a reload, and a
  // second session gets the same confirmed size.
  await sizeForm(a.page).getByLabel("Width").fill("800");
  await sizeForm(a.page).getByLabel("Height").fill("600");
  await a.page.reload();
  await expectConfirmed(a.page, 3840, 2160);
  const a2 = await signedIn(browser, alice);
  await expectConfirmed(a2.page, 3840, 2160);

  // S4 owner only: B, with a valid session and CSRF token, cannot resize A's
  // canvas, and A's canvas is unchanged.
  const aliceCanvas = await ownCanvas(a.page);
  const b = await signedIn(browser, bob);
  const crossResize = await b.page.evaluate(
    async ({ tenantId, canvasId }) => {
      const session = await (
        await fetch("/api/session", { credentials: "same-origin" })
      ).json();
      const response = await fetch(
        `/api/tenants/${tenantId}/canvases/${canvasId}/dimensions`,
        {
          method: "PUT",
          credentials: "same-origin",
          headers: {
            "Content-Type": "application/json",
            "X-CSRF-Token": session.csrf_token,
          },
          body: JSON.stringify({
            operation_id: crypto.randomUUID(),
            width: 640,
            height: 360,
          }),
        },
      );
      return { status: response.status, body: await response.text() };
    },
    { tenantId: aliceCanvas.tenant_id, canvasId: aliceCanvas.canvas_id },
  );
  expect(crossResize.status).toBe(403);
  expect(crossResize.body).not.toContain(aliceCanvas.canvas_id);
  await a.page.reload();
  await expectConfirmed(a.page, 3840, 2160);
  expect(await ownCanvas(a.page)).toEqual(aliceCanvas);

  // S7 isolation: B's own canvas kept the initial size.
  await expectConfirmed(b.page, 1920, 1080);

  for (const session of [op, a, a2, b]) {
    await session.context.close();
  }
});
