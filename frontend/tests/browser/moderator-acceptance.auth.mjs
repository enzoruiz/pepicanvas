import { spawnSync } from "node:child_process";
import { randomBytes } from "node:crypto";
import path from "node:path";

import { expect, test } from "@playwright/test";

const password = `e2e-password-${randomBytes(6).toString("hex")}`;
const backendRoot = path.resolve(import.meta.dirname, "../../../backend");

const inactive =
  "Your account is not active yet. Ask an operator to activate it.";
const signInNote =
  "Sign in or create an account to accept the moderator invitation.";
const invitedText = "You were invited to moderate a streamer's canvas.";
const acceptedText = "You are now a moderator of this canvas.";
const unusableText =
  "This invitation link can't be used. Ask the streamer for a new one.";

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

// Records the URL and Referer of every request a page sends, so the test
// can prove the invitation token never leaves the fragment.
function recordRequests(page, sent) {
  page.on("request", (request) => {
    sent.push({
      url: request.url(),
      referer: request.headers()["referer"] ?? "",
    });
  });
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

async function signIn(page, email) {
  await page.getByLabel("Email", { exact: true }).fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByText(`Signed in as ${email}`)).toBeVisible();
}

// Signs in an account in its own browser context, so sessions stay apart.
async function signedIn(browser, email, sent) {
  const context = await browser.newContext();
  const page = await context.newPage();
  recordRequests(page, sent);
  await openEditor(page);
  await signIn(page, email);
  return { context, page };
}

function moderatedRegion(page) {
  return page.getByRole("region", { name: "Canvases you moderate" });
}

function canvasReads(requests) {
  return requests.filter(({ url }) => {
    const { pathname } = new URL(url);
    return (
      pathname === "/api/canvas" ||
      pathname === "/api/memberships" ||
      pathname.startsWith("/api/tenants/")
    );
  });
}

test("an invited account signs in, accepts once, and moderates the canvas read-only", async ({
  browser,
  page,
}) => {
  const operator = uniqueEmail("olga");
  const alice = uniqueEmail("alice");
  const bob = uniqueEmail("bob");
  const sent = [];

  await openEditor(page);
  for (const email of [operator, alice, bob]) {
    await register(page, email);
  }
  grantOperator(operator);

  const op = await signedIn(browser, operator, sent);
  const activationPanel = op.page.getByRole("region", {
    name: "Manage streamers",
  });
  await activationPanel.getByLabel("Streamer email").fill(alice);
  await activationPanel
    .getByRole("button", { name: "Activate", exact: true })
    .click();
  await expect(activationPanel.getByRole("status")).toHaveText(
    "Streamer activated.",
  );

  const a = await signedIn(browser, alice, sent);
  const invitations = a.page.getByRole("region", { name: "Invite moderators" });
  await expect(invitations.getByText("No pending invitations.")).toBeVisible();
  const ownCanvas = await a.page.request.get("/api/canvas");
  expect(ownCanvas.status()).toBe(200);
  const aliceCanvas = await ownCanvas.json();
  await invitations
    .getByRole("button", { name: "Create invitation link" })
    .click();
  const link = await invitations.getByLabel("Invitation link").inputValue();
  expect(link).toMatch(
    /^http:\/\/localhost:4176\/editor\/#pci_[A-Za-z0-9_-]{43}$/,
  );

  // S1: without a session the link shows only the sign-in forms and a note;
  // no canvas or membership is requested and no canvas state is shown.
  const bContext = await browser.newContext();
  const invited = await bContext.newPage();
  const bobRequests = [];
  recordRequests(invited, sent);
  recordRequests(invited, bobRequests);
  await invited.goto(link);
  await expect(
    invited.getByRole("heading", { level: 2, name: "Sign in" }),
  ).toBeVisible();
  await expect(invited.getByText(signInNote)).toBeVisible();
  await expect(
    invited.getByRole("button", { name: "Accept invitation" }),
  ).toHaveCount(0);
  await expect(invited.locator("[data-canvas-surface]")).toHaveCount(0);
  await invited.getByRole("button", { name: "Create an account" }).click();
  await expect(invited.getByText(signInNote)).toBeVisible();
  await invited
    .getByRole("button", { name: "I already have an account" })
    .click();
  expect(canvasReads(bobRequests)).toEqual([]);

  // Signing in keeps the fragment; acceptance waits for an explicit click.
  await signIn(invited, bob);
  await expect(invited.getByText(invitedText)).toBeVisible();
  expect(new URL(invited.url()).hash).toMatch(/^#pci_/);
  await expect(
    invited.getByRole("region", { name: "Your canvas" }).getByText(inactive),
  ).toBeVisible();
  expect(
    bobRequests.some(({ url }) => url.endsWith("/api/invitations/accept")),
  ).toBe(false);

  // S2: the confirmation grants the membership and shows the canvas.
  await invited.getByRole("button", { name: "Accept invitation" }).click();
  await expect(invited.getByText(acceptedText)).toBeVisible();
  const moderated = moderatedRegion(invited);
  await expect(
    moderated.getByText(`${aliceCanvas.width} × ${aliceCanvas.height}`),
  ).toBeVisible();
  await expect(moderated.locator("[data-canvas-surface]")).toHaveCount(1);
  expect(invited.url()).not.toContain("pci_");
  expect(invited.url()).toBe("http://localhost:4176/editor/");

  // S6: a reload without the fragment reflects the confirmed membership.
  await invited.reload();
  await expect(
    moderatedRegion(invited).getByText(
      `${aliceCanvas.width} × ${aliceCanvas.height}`,
    ),
  ).toBeVisible();
  await expect(
    invited.getByRole("button", { name: "Accept invitation" }),
  ).toHaveCount(0);

  // S8 in the editor: no size form, no invitations panel, and a direct
  // resize with a valid session and CSRF token is refused.
  await expect(invited.getByLabel("Width")).toHaveCount(0);
  await expect(invited.getByRole("button", { name: "Apply size" })).toHaveCount(
    0,
  );
  await expect(
    invited.getByRole("region", { name: "Invite moderators" }),
  ).toHaveCount(0);
  const crossResize = await invited.evaluate(
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
      return { status: response.status };
    },
    { tenantId: aliceCanvas.tenant_id, canvasId: aliceCanvas.canvas_id },
  );
  expect(crossResize.status).toBe(403);
  expect((await a.page.request.get("/api/canvas")).status()).toBe(200);
  expect(await (await a.page.request.get("/api/canvas")).json()).toEqual(
    aliceCanvas,
  );

  // S2/S7: the consumed link cannot be used again. Leaving the page first
  // makes the next visit a full load instead of a fragment navigation.
  await invited.goto("about:blank");
  await invited.goto(link);
  await expect(invited.getByText(invitedText)).toBeVisible();
  await invited.getByRole("button", { name: "Accept invitation" }).click();
  await expect(invited.getByText(unusableText)).toBeVisible();
  expect(invited.url()).not.toContain("pci_");
  await expect(
    moderatedRegion(invited).locator("[data-canvas-surface]"),
  ).toHaveCount(1);

  // The owner sees the invitation gone from the pending list.
  await a.page.reload();
  await expect(invitations.getByText("No pending invitations.")).toBeVisible();

  // S10: the token never reached the server in a URL or a Referer header.
  expect(sent.length).toBeGreaterThan(0);
  expect(sent.some(({ url }) => url.endsWith("/api/invitations/accept"))).toBe(
    true,
  );
  for (const request of sent) {
    expect(request.url).not.toContain("pci_");
    expect(request.referer).not.toContain("pci_");
  }

  for (const context of [op.context, a.context, bContext]) {
    await context.close();
  }
});
