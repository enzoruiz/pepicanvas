import { describe, expect, it } from "vitest";

import { createRenderRegistry } from "./renderRegistry";

const mark = { renderedAtMs: 12.5, correlationId: "c".repeat(32), sequence: 4 };

describe("createRenderRegistry", () => {
  it("resolves a wait made before the render", async () => {
    const registry = createRenderRegistry();
    const waiting = registry.rendered("op-1");

    registry.record("op-1", mark);

    await expect(waiting).resolves.toEqual(mark);
  });

  it("resolves a wait made after the render", async () => {
    const registry = createRenderRegistry();
    registry.record("op-1", mark);

    await expect(registry.rendered("op-1")).resolves.toEqual(mark);
  });

  it("keeps other operations pending", async () => {
    const registry = createRenderRegistry();
    registry.record("op-1", mark);
    let settled = false;
    void registry.rendered("op-2").then(() => (settled = true));

    await Promise.resolve();
    expect(settled).toBe(false);
  });

  it("rejects the wait of an operation whose render failed", async () => {
    const registry = createRenderRegistry();
    const waiting = registry.rendered("op-1");

    registry.fail("op-1", new Error("play refused"));

    await expect(waiting).rejects.toThrow("play refused");
    await expect(registry.rendered("op-1")).rejects.toThrow("play refused");
  });
});
