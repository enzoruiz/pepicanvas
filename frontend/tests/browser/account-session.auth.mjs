import { randomBytes } from "node:crypto";

import { expect, test } from "@playwright/test";

const password = `e2e-password-${randomBytes(6).toString("hex")}`;

function uniqueEmail(label) {
  return `${label}-${randomBytes(6).toString("hex")}@example.com`;
}

async function openEditor(page) {
  await page.goto("/editor/");
  await expect(
    page.getByRole("heading", { level: 1, name: "PepiCanvas editor" }),
  ).toBeVisible();
}

async function register(page, email) {
  await page.getByRole("button", { name: "Create an account" }).click();
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Create account" }).click();
}

async function signIn(page, email, pass = password) {
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
}

// Every request stays on the editor origin and no URL carries a credential.
function watchRequests(page, secrets) {
  const seen = [];
  page.on("request", (request) => seen.push(request.url()));
  return () => {
    for (const url of seen) {
      expect(new URL(url).origin).toBe("http://localhost:4176");
      for (const secret of secrets()) {
        expect(url).not.toContain(secret);
      }
    }
  };
}

test("register, sign in, and keep the session across a reload", async ({
  page,
  context,
}) => {
  const email = uniqueEmail("alice");
  let token = "";
  const checkRequests = watchRequests(page, () => [password, email, token]);

  await openEditor(page);
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await register(page, email);
  await expect(page.getByRole("status")).toHaveText(
    "Account created. You can sign in now.",
  );

  await signIn(page, email);
  await expect(page.getByText(`Signed in as ${email}`)).toBeVisible();

  const cookies = await context.cookies();
  const session = cookies.find((cookie) => cookie.name === "__Host-pcs");
  expect(session).toMatchObject({
    httpOnly: true,
    secure: true,
    sameSite: "Lax",
    path: "/",
  });
  token = session.value;
  expect(await page.evaluate(() => document.cookie)).not.toContain(token);

  // Reconnection: a reload asks the server again and keeps the valid session.
  await page.reload();
  await expect(page.getByText(`Signed in as ${email}`)).toBeVisible();
  expect(await page.content()).not.toContain(password);
  checkRequests();
});

test("an identity cannot be registered twice", async ({ page }) => {
  const email = uniqueEmail("bob");
  await openEditor(page);
  await register(page, email);
  await expect(page.getByRole("status")).toBeVisible();

  await register(page, email.toUpperCase());
  await expect(page.getByRole("alert")).toHaveText(
    "An account with this email already exists.",
  );
});

test("invalid credentials show a safe message and create no session", async ({
  page,
  context,
}) => {
  const email = uniqueEmail("carol");
  await openEditor(page);
  await register(page, email);
  await expect(page.getByRole("status")).toBeVisible();

  for (const [who, pass] of [
    [email, `wrong-${password}`],
    [uniqueEmail("nobody"), password],
  ]) {
    await signIn(page, who, pass);
    await expect(page.getByRole("alert")).toHaveText(
      "Email or password is incorrect.",
    );
  }
  expect(
    (await context.cookies()).some((cookie) => cookie.name === "__Host-pcs"),
  ).toBe(false);
  expect(await page.content()).not.toContain(password);
});

test("an ended session is not restored from the back/forward cache", async ({
  page,
  context,
}) => {
  const email = uniqueEmail("dana");
  await openEditor(page);
  await register(page, email);
  await expect(page.getByRole("status")).toBeVisible();
  await signIn(page, email);
  await expect(page.getByText(`Signed in as ${email}`)).toBeVisible();

  // Leave the signed-in document, end the session elsewhere, and come back.
  await page.goto("/overlay/");
  const other = await context.newPage();
  await openEditor(other);
  await other.getByRole("button", { name: "Sign out" }).click();
  await expect(other.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await other.close();

  await page.goBack();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  await expect(page.getByText(`Signed in as ${email}`)).toHaveCount(0);
});
