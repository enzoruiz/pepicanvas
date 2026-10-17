import { describe, expect, it } from "vitest";

import { classifyClientSupport, type ClientSnapshot } from "./clientSupport";

const chromeUserAgent =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
  "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36";
const edgeUserAgent = `${chromeUserAgent} Edg/140.0.0.0`;
const obsCurrentUserAgent =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
  "(KHTML, like Gecko) Chrome/127.0.6533.120 OBS/32.2.2 Safari/537.36";
const obsPreviousUserAgent = obsCurrentUserAgent.replace(
  "OBS/32.2.2",
  "OBS/32.2.1",
);

function snapshot(overrides: Partial<ClientSnapshot> = {}): ClientSnapshot {
  return {
    userAgent: chromeUserAgent,
    userAgentDataMobile: false,
    maxTouchPoints: 0,
    obsStudioIntegration: false,
    viewportWidth: 1280,
    viewportHeight: 720,
    ...overrides,
  };
}

describe("classifyClientSupport", () => {
  it.each([
    ["desktop Google Chrome", chromeUserAgent, "chrome"],
    ["desktop Microsoft Edge", edgeUserAgent, "edge"],
  ] as const)("supports %s", (_label, userAgent, family) => {
    expect(classifyClientSupport(snapshot({ userAgent }))).toEqual({
      supported: true,
      browserFamily: family,
      reasons: [],
    });
  });

  it.each([
    ["current OBS", obsCurrentUserAgent],
    ["previous OBS", obsPreviousUserAgent],
  ])("supports %s Browser Source with its integration", (_label, userAgent) => {
    expect(
      classifyClientSupport(
        snapshot({ userAgent, obsStudioIntegration: true }),
      ),
    ).toEqual({
      supported: true,
      browserFamily: "obs",
      reasons: [],
    });
  });

  it.each([
    ["OBS identity without integration", obsCurrentUserAgent, false],
    ["ordinary Chrome with OBS integration", chromeUserAgent, true],
    ["OBS token after Safari", `${chromeUserAgent} OBS/32.2.2`, true],
    [
      "OBS token before Chrome",
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
        "(KHTML, like Gecko) OBS/32.2.2 Chrome/127.0.6533.120 Safari/537.36",
      true,
    ],
    ["extra OBS suffix", `${obsCurrentUserAgent} Custom/1.0`, true],
    [
      "nonnumeric OBS version",
      obsCurrentUserAgent.replace("OBS/32.2.2", "OBS/current"),
      true,
    ],
  ])("rejects %s", (_label, userAgent, obsStudioIntegration) => {
    const result = classifyClientSupport(
      snapshot({ userAgent, obsStudioIntegration }),
    );

    expect(result.supported).toBe(false);
    expect(result.browserFamily).toBe("unknown");
    expect(result.reasons).toContain("browser");
  });

  it.each([
    ["mobile hint", { userAgentDataMobile: true }, "device"],
    [
      "Android tablet identity",
      {
        userAgent: obsCurrentUserAgent.replace(
          "Windows NT 10.0; Win64; x64",
          "Linux; Android 15; Pixel Tablet",
        ),
      },
      "device",
    ],
    [
      "iPad desktop mode",
      {
        userAgent: obsCurrentUserAgent.replace(
          "Windows NT 10.0; Win64; x64",
          "Macintosh; Intel Mac OS X 10_15",
        ),
        maxTouchPoints: 5,
      },
      "device",
    ],
    ["undersized width", { viewportWidth: 1279 }, "viewport"],
    ["undersized height", { viewportHeight: 719 }, "viewport"],
  ] as const)(
    "applies the %s guard to OBS Browser Source",
    (_label, overrides, reason) => {
      const result = classifyClientSupport(
        snapshot({
          userAgent: obsCurrentUserAgent,
          obsStudioIntegration: true,
          ...overrides,
        }),
      );

      expect(result.supported).toBe(false);
      expect(result.browserFamily).toBe("obs");
      expect(result.reasons).toContain(reason);
    },
  );

  it("accepts the 1280x720 viewport boundary", () => {
    expect(
      classifyClientSupport(
        snapshot({ viewportWidth: 1280, viewportHeight: 720 }),
      ).supported,
    ).toBe(true);
  });

  it.each([
    ["narrow", 1279, 720],
    ["short", 1280, 719],
  ])("rejects an undersized %s viewport", (_label, width, height) => {
    expect(
      classifyClientSupport(
        snapshot({ viewportWidth: width, viewportHeight: height }),
      ),
    ).toMatchObject({ supported: false, reasons: ["viewport"] });
  });

  it.each([
    ["UA Client Hint mobile", { userAgentDataMobile: true }],
    [
      "Android tablet",
      {
        userAgent:
          "Mozilla/5.0 (Linux; Android 15; Pixel Tablet) AppleWebKit/537.36 " +
          "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
      },
    ],
    [
      "iPad desktop mode",
      {
        userAgent:
          "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15) AppleWebKit/537.36 " +
          "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
        userAgentDataMobile: false,
        maxTouchPoints: 5,
      },
    ],
  ])("rejects %s", (_label, overrides) => {
    const result = classifyClientSupport(snapshot(overrides));

    expect(result.supported).toBe(false);
    expect(result.reasons).toContain("device");
  });

  it.each([
    [
      "Firefox",
      "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:142.0) Gecko/20100101 Firefox/142.0",
    ],
    [
      "Safari",
      "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 " +
        "(KHTML, like Gecko) Version/18.6 Safari/605.1.15",
    ],
    ["unknown Chromium derivative", `${chromeUserAgent} Custom/1.0`],
    ["malformed identity", "Chrome definitely"],
    ["unknown identity", ""],
  ])("rejects %s", (_label, userAgent) => {
    const result = classifyClientSupport(snapshot({ userAgent }));

    expect(result.supported).toBe(false);
    expect(result.browserFamily).toBe("unknown");
    expect(result.reasons).toContain("browser");
  });
});
