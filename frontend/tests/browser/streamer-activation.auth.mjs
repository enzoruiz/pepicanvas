import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";

import { expect, test } from "@playwright/test";

const password = `e2e-password-${randomBytes(6).toString("hex")}`;
const backendRoot = path.resolve(import.meta.dirname, "../../../backend");

const activated = "Streamer activated.";
const unchanged = "Streamer is active; the canvas is unchanged.";
const deactivated = "Streamer deactivated. Their canvas and files are kept.";
const alreadyInactive = "Streamer was already inactive; nothing changed.";
const inactive =
  "Your account is not active yet. Ask an operator to activate it.";
const subscriptionInactive =
  "Your subscription is inactive. Your canvas and files are kept; ask an operator to reactivate it.";

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
  expect(result.stdout).toMatch(/^operator granted: [0-9a-f]{32}\n$/);
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

async function activate(page, email, message) {
  await submitStreamer(page, "Activate", email, message);
}

async function deactivate(page, email, message) {
  await submitStreamer(page, "Deactivate", email, message);
}

async function submitStreamer(page, action, email, message) {
  const panel = page.getByRole("region", { name: "Manage streamers" });
  await panel.getByLabel("Streamer email").fill(email);
  await panel.getByRole("button", { name: action, exact: true }).click();
  await expect(panel.getByRole("status")).toHaveText(message);
  await expect(panel.getByLabel("Streamer email")).toHaveValue("");
}

async function ownCanvas(page) {
  const response = await page.request.get("/api/canvas");
  expect(response.status()).toBe(200);
  return response.json();
}

test("an operator activates and deactivates streamers who each open only their own canvas", async ({
  browser,
  page,
}) => {
  const operator = uniqueEmail("olga");
  const alice = uniqueEmail("alice");
  const bob = uniqueEmail("bob");
  const carol = uniqueEmail("carol");

  await openEditor(page);
  for (const email of [operator, alice, bob, carol]) {
    await register(page, email);
  }
  grantOperator(operator);

  const op = await signedIn(browser, operator);
  const canvasRegion = op.page.getByRole("region", { name: "Your canvas" });
  await expect(canvasRegion.getByText(inactive)).toBeVisible();
  await activate(op.page, alice, activated);
  await activate(op.page, bob.toUpperCase(), activated);

  const a = await signedIn(browser, alice);
  await expect(
    a.page
      .getByRole("region", { name: "Your canvas" })
      .getByText("1920 × 1080"),
  ).toBeVisible();
  await expect(
    a.page.getByRole("region", { name: "Manage streamers" }),
  ).toHaveCount(0);
  const aliceCanvas = await ownCanvas(a.page);
  expect(aliceCanvas).toMatchObject({ width: 1920, height: 1080, revision: 0 });

  // A repeated activation keeps the same canvas.
  await activate(op.page, alice, unchanged);
  expect(await ownCanvas(a.page)).toEqual(aliceCanvas);

  const b = await signedIn(browser, bob);
  await expect(
    b.page
      .getByRole("region", { name: "Your canvas" })
      .getByText("1920 × 1080"),
  ).toBeVisible();
  const bobCanvas = await ownCanvas(b.page);
  expect(bobCanvas.tenant_id).not.toBe(aliceCanvas.tenant_id);
  expect(bobCanvas.canvas_id).not.toBe(aliceCanvas.canvas_id);
  expect(JSON.stringify(bobCanvas)).not.toContain(aliceCanvas.canvas_id);

  // No cross read: B cannot get A's canvas, while A can.
  const route = `/api/tenants/${aliceCanvas.tenant_id}/canvases/${aliceCanvas.canvas_id}`;
  const crossRead = await b.page.request.get(route);
  expect(crossRead.status()).toBe(403);
  expect(await crossRead.text()).not.toContain(aliceCanvas.canvas_id);
  expect((await a.page.request.get(route)).status()).toBe(200);

  const c = await signedIn(browser, carol);
  await expect(
    c.page.getByRole("region", { name: "Your canvas" }).getByText(inactive),
  ).toBeVisible();
  await expect(c.page.getByText("×")).toHaveCount(0);

  // Deactivation cuts A's access and keeps the canvas; B continues.
  await deactivate(op.page, alice, deactivated);
  const aliceRegion = a.page.getByRole("region", { name: "Your canvas" });
  await a.page.reload();
  await expect(aliceRegion.getByRole("status")).toHaveText(
    subscriptionInactive,
  );
  await expect(aliceRegion.getByText("×")).toHaveCount(0);
  const denied = await a.page.request.get("/api/canvas");
  expect(denied.status()).toBe(403);
  const deniedBody = await denied.text();
  expect(JSON.parse(deniedBody)).toMatchObject({
    error: "subscription_inactive",
  });
  for (const secret of [alice, aliceCanvas.tenant_id, aliceCanvas.canvas_id]) {
    expect(deniedBody).not.toContain(secret);
  }
  expect((await a.page.request.get(route)).status()).toBe(403);

  const bobRegion = b.page.getByRole("region", { name: "Your canvas" });
  await b.page.reload();
  await expect(bobRegion.getByText("1920 × 1080")).toBeVisible();
  expect(await ownCanvas(b.page)).toEqual(bobCanvas);

  await deactivate(op.page, alice, alreadyInactive);

  // Reactivation is the activation and brings back the same canvas.
  await activate(op.page, alice, unchanged);
  await a.page.reload();
  await expect(aliceRegion.getByText("1920 × 1080")).toBeVisible();
  await expect(aliceRegion.getByText(subscriptionInactive)).toHaveCount(0);
  expect(await ownCanvas(a.page)).toEqual(aliceCanvas);

  for (const session of [op, a, b, c]) {
    await session.context.close();
  }
});
