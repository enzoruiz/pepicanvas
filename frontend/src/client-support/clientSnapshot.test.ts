import { describe, expect, it } from "vitest";

import { readClientSnapshot } from "./clientSnapshot";

describe("readClientSnapshot", () => {
  it("captures browser, device, and viewport inputs without classifying them", () => {
    const result = readClientSnapshot(
      {
        userAgent: "example-agent",
        userAgentData: { mobile: true },
        maxTouchPoints: 3,
      },
      { innerWidth: 1440, innerHeight: 900, obsstudio: {} },
    );

    expect(result).toEqual({
      userAgent: "example-agent",
      userAgentDataMobile: true,
      maxTouchPoints: 3,
      obsStudioIntegration: true,
      viewportWidth: 1440,
      viewportHeight: 900,
    });
  });

  it("recognizes function-valued OBS integration", () => {
    expect(
      readClientSnapshot(
        { userAgent: "example-agent", maxTouchPoints: 0 },
        {
          innerWidth: 1280,
          innerHeight: 720,
          obsstudio: () => undefined,
        },
      ).obsStudioIntegration,
    ).toBe(true);
  });

  it.each([null, undefined, "present", 1])(
    "rejects non-object OBS integration value %s",
    (obsstudio) => {
      expect(
        readClientSnapshot(
          { userAgent: "example-agent", maxTouchPoints: 0 },
          { innerWidth: 1280, innerHeight: 720, obsstudio },
        ).obsStudioIntegration,
      ).toBe(false);
    },
  );

  it("uses a null mobile hint when userAgentData is unavailable", () => {
    expect(
      readClientSnapshot(
        { userAgent: "example-agent", maxTouchPoints: 0 },
        { innerWidth: 1280, innerHeight: 720 },
      ).userAgentDataMobile,
    ).toBeNull();
  });
});
