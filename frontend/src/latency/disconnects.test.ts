import { describe, expect, it } from "vitest";

import { trackDisconnects } from "./disconnects";

describe("trackDisconnects", () => {
  it("counts every error event and nothing else", () => {
    const source = new EventTarget();
    const disconnects = trackDisconnects(source);

    expect(disconnects()).toBe(0);
    source.dispatchEvent(new Event("error"));
    source.dispatchEvent(new Event("open"));
    source.dispatchEvent(new Event("message"));
    expect(disconnects()).toBe(1);
    source.dispatchEvent(new Event("error"));
    expect(disconnects()).toBe(2);
  });
});
