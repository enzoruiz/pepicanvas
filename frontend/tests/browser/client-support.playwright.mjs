import { expect, test } from "@playwright/test";

const expectedVersions = {
  "chrome-current": "155.0.8059.79",
  "chrome-previous": "154.0.8037.92",
  "edge-current": "155.0.4283.45",
  "edge-previous": "154.0.4258.62",
};

test("runs the complete client support scenario on the exact browser", async ({
  browser,
  page,
}, testInfo) => {
  expect(browser.version()).toBe(expectedVersions[testInfo.project.name]);

  await page.setViewportSize({ width: 1280, height: 720 });
  await page.goto("/editor/");
  await expect(
    page.getByRole("heading", { name: "PepiCanvas editor" }),
  ).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);

  await page.setViewportSize({ width: 1279, height: 720 });
  await expect(page.getByRole("alert")).toContainText("Unsupported client");
  await expect(page.getByTestId("client-support-content")).toHaveAttribute(
    "aria-hidden",
    "true",
  );

  await page.setViewportSize({ width: 1280, height: 720 });
  await expect(page.getByRole("alert")).toHaveCount(0);
  await expect(
    page.getByRole("heading", { name: "PepiCanvas editor" }),
  ).toBeVisible();

  await page.goto("/overlay/");
  await expect(
    page.getByRole("heading", { name: "PepiCanvas overlay" }),
  ).toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
});
