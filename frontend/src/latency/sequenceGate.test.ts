import { describe, expect, it } from "vitest";

import { createRenderRegistry } from "./renderRegistry";
import { createSequenceGate } from "./sequenceGate";

describe("createSequenceGate", () => {
  it("takes each sequence once and drops repetitions before and after a reconnection", () => {
    const take = createSequenceGate();

    expect([1, 2, 2, 3].map(take)).toEqual([true, true, false, true]);
    // The reconnection replays 2 and 3 before the live 4.
    expect([2, 3, 4, 4].map(take)).toEqual([false, false, true, false]);
  });

  it("records one render mark per sequence when a replay repeats it", async () => {
    const take = createSequenceGate();
    const registry = createRenderRegistry();
    const recorded: number[] = [];
    const deliver = (sequence: number, renderedAtMs: number) => {
      if (!take(sequence)) return;
      recorded.push(sequence);
      registry.record(`op-${sequence}`, {
        renderedAtMs,
        correlationId: "0123456789abcdef0123456789abcdef",
        sequence,
      });
    };

    deliver(1, 10);
    deliver(1, 20);
    deliver(2, 30);
    deliver(1, 40);

    expect(recorded).toEqual([1, 2]);
    await expect(registry.rendered("op-1")).resolves.toMatchObject({
      renderedAtMs: 10,
    });
  });
});
