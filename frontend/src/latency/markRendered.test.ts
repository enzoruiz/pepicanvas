import { describe, expect, it } from "vitest";

import { markRendered } from "./markRendered";

describe("markRendered", () => {
  it("takes the time in the second animation frame after the commit", async () => {
    const frames: FrameRequestCallback[] = [];
    const raf = (callback: FrameRequestCallback) => frames.push(callback);
    let clock = 10;
    const mark = markRendered(raf, () => clock);

    expect(frames).toHaveLength(1);
    clock = 20;
    frames.shift()?.(20);
    expect(frames).toHaveLength(1);
    clock = 36.5;
    frames.shift()?.(36.5);

    await expect(mark).resolves.toBe(36.5);
    expect(frames).toHaveLength(0);
  });
});
