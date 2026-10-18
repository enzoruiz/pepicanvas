/* global fetch, URL, window, performance */

import { mkdir, writeFile } from "node:fs/promises";

import { expect, test } from "@playwright/test";

const fixture = "http://127.0.0.1:4174";
const decoysPath = new URL(
  "../../.cache/privacy-fixture/decoys.json",
  import.meta.url,
);

async function loadDecoys(request) {
  const response = await request.get(`${fixture}/__fixture/decoys`);
  expect(response.ok()).toBe(true);
  return response.json();
}

// The fixture generates fresh decoys on every start; record them so the
// privacy runner can scan this run's evidence and prove rotation.
test.beforeAll(async () => {
  const response = await fetch(`${fixture}/__fixture/decoys`);
  expect(response.ok).toBe(true);
  await mkdir(new URL(".", decoysPath), { recursive: true });
  await writeFile(decoysPath, await response.text(), { mode: 0o600 });
});

test.beforeEach(async ({ request }) => {
  await request.post(`${fixture}/__fixture/restore`);
});

test("private response is neither reloaded nor served from HTTP cache after an access cut", async ({
  page,
  request,
}) => {
  const decoys = await loadDecoys(request);
  await page.route("https://third-party.invalid/**", (route) =>
    route.fulfill({ status: 204 }),
  );

  await page.goto(`${fixture}/`);
  await page.click("#open");
  await expect(page.locator("#private")).toHaveText(decoys.private_marker);
  const privateURL = page.url();

  const cut = await request.post(`${fixture}/__fixture/cut`);
  expect(cut.status()).toBe(204);

  const reloaded = await page.reload();
  expect(reloaded.status()).toBe(401);
  expect(await page.content()).not.toContain(decoys.private_marker);
  await expect(page.locator("body")).toContainText('"error":"unauthorized"');

  await page.goto(`${fixture}/`);
  const cached = await page.evaluate(async (url) => {
    const response = await fetch(url, { cache: "force-cache" });
    return { status: response.status, body: await response.text() };
  }, privateURL);
  expect(cached.status).toBe(401);
  expect(cached.body).not.toContain(decoys.private_marker);
});

test("private page sends no Referer to third parties and no secret leaves through outbound URLs", async ({
  page,
  request,
}) => {
  const decoys = await loadDecoys(request);
  const outbound = [];
  await page.route("https://third-party.invalid/**", (route) => {
    outbound.push({
      url: route.request().url(),
      referer: route.request().headers()["referer"] ?? null,
    });
    return route.fulfill({ status: 204 });
  });

  await page.goto(`${fixture}/private/canvas?invite=${decoys.invitation}`);
  await expect(page.locator("#private")).toHaveText(decoys.private_marker);

  expect(outbound).toHaveLength(1);
  expect(outbound[0].referer).toBeNull();
  for (const value of [
    decoys.invitation,
    decoys.session,
    decoys.obs_credential,
  ]) {
    expect(outbound[0].url).not.toContain(value);
  }
});

for (const app of ["editor", "overlay"]) {
  test(`${app} document requests only its own origin and disables referrers`, async ({
    page,
    baseURL,
  }) => {
    const origins = new Set();
    page.on("request", (req) => origins.add(new URL(req.url()).origin));

    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto(`/${app}/`);
    await expect(
      page.getByRole("heading", { name: `PepiCanvas ${app}` }),
    ).toBeVisible();
    await page.waitForLoadState("networkidle");

    expect([...origins]).toEqual([new URL(baseURL).origin]);
    await expect(page.locator('meta[name="referrer"]')).toHaveAttribute(
      "content",
      "no-referrer",
    );
  });
}

for (const app of ["editor", "overlay"]) {
  test(`${app} is reloaded instead of restored from the back/forward cache`, async ({
    page,
  }) => {
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto(`/${app}/`);
    await expect(
      page.getByRole("heading", { name: `PepiCanvas ${app}` }),
    ).toBeVisible();
    await page.evaluate(() => {
      window.__privateState = "held-before-navigation";
    });

    await page.goto(`${fixture}/`);
    await page.goBack({ waitUntil: "commit" });
    await expect(
      page.getByRole("heading", { name: `PepiCanvas ${app}` }),
    ).toBeVisible();

    expect(await page.evaluate(() => window.__privateState)).toBeUndefined();
    expect(
      await page.evaluate(
        () => performance.getEntriesByType("navigation")[0].type,
      ),
    ).toBe("reload");
  });
}
