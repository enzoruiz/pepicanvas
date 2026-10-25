import { describe, expect, it, vi } from "vitest";

import { measurePings } from "./pings";

function reply(status: number, body: string): Response {
  return new Response(body, { status });
}

describe("measurePings", () => {
  it("sends one ping at a time and stamps the client clock around each", async () => {
    const events: string[] = [];
    let clock = 0;
    const now = () => {
      clock += 1;
      events.push(`now:${clock}`);
      return clock;
    };
    let served = 0;
    const fetchPing = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      served += 1;
      events.push(`fetch:${input as string}:${init?.cache}`);
      return Promise.resolve(
        reply(200, JSON.stringify({ serverAtNs: served * 1_000_000 })),
      );
    });

    const pings = await measurePings(fetchPing, now, 3);

    expect(pings).toEqual([
      { clientSentMs: 1, serverAtNs: 1_000_000, clientReceivedMs: 2 },
      { clientSentMs: 3, serverAtNs: 2_000_000, clientReceivedMs: 4 },
      { clientSentMs: 5, serverAtNs: 3_000_000, clientReceivedMs: 6 },
    ]);
    expect(events).toEqual([
      "now:1",
      "fetch:/probe/ping:no-store",
      "now:2",
      "now:3",
      "fetch:/probe/ping:no-store",
      "now:4",
      "now:5",
      "fetch:/probe/ping:no-store",
      "now:6",
    ]);
  });

  it("does not start the next ping before the previous one answered", async () => {
    let release: (() => void) | undefined;
    const fetchPing = vi.fn(
      () =>
        new Promise<Response>((resolve) => {
          release = () => resolve(reply(200, '{"serverAtNs":5}'));
        }),
    );
    const pings = measurePings(fetchPing, () => 0, 2);

    await Promise.resolve();
    expect(fetchPing).toHaveBeenCalledTimes(1);
    release?.();
    await vi.waitFor(() => expect(fetchPing).toHaveBeenCalledTimes(2));
    release?.();
    await expect(pings).resolves.toHaveLength(2);
  });

  it.each([
    ["a non-200 status", reply(503, '{"serverAtNs":5}')],
    ["a body that is not JSON", reply(200, "not json")],
    ["a body without serverAtNs", reply(200, "{}")],
    ["a non-integer serverAtNs", reply(200, '{"serverAtNs":1.5}')],
    ["a negative serverAtNs", reply(200, '{"serverAtNs":-1}')],
    ["a string serverAtNs", reply(200, '{"serverAtNs":"5"}')],
    ["a body that is not an object", reply(200, "[5]")],
  ])("rejects %s", async (_name, response) => {
    const fetchPing = vi.fn(() => Promise.resolve(response));

    await expect(measurePings(fetchPing, () => 0, 3)).rejects.toThrow(/ping/);
    expect(fetchPing).toHaveBeenCalledTimes(1);
  });

  it("rejects a count that is not a positive integer", async () => {
    const fetchPing = vi.fn();

    await expect(measurePings(fetchPing, () => 0, 0)).rejects.toThrow();
    expect(fetchPing).not.toHaveBeenCalled();
  });
});
