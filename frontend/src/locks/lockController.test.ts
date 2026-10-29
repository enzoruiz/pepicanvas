import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
  Acquisition,
  LockError,
  LockResult,
  LockSessionInfo,
  Renewal,
} from "./api";
import { type LockClient, LockController } from "./lockController";

type Answer<T> = LockResult<T> | Promise<LockResult<T>>;

// A client whose answers each test queues in order.
function fakeClient() {
  const queues = {
    open: [] as Answer<LockSessionInfo>[],
    heartbeat: [] as Answer<Renewal>[],
    acquire: [] as Answer<Acquisition>[],
    release: [] as Answer<undefined>[],
  };
  function next<T>(queue: Answer<T>[], name: string): Promise<LockResult<T>> {
    const answer = queue.shift();
    if (answer === undefined) throw new Error(`unexpected ${name}`);
    return Promise.resolve(answer);
  }
  const client = {
    open: vi.fn<LockClient["open"]>(() => next(queues.open, "open")),
    heartbeat: vi.fn<LockClient["heartbeat"]>(() =>
      next(queues.heartbeat, "heartbeat"),
    ),
    acquire: vi.fn<LockClient["acquire"]>(() =>
      next(queues.acquire, "acquire"),
    ),
    release: vi.fn<LockClient["release"]>(() =>
      next(queues.release, "release"),
    ),
    end: vi.fn<LockClient["end"]>(() =>
      Promise.resolve({ ok: true, value: undefined }),
    ),
  } satisfies LockClient;
  return { client, queues };
}

function session(id: string, heartbeatIntervalMs = 1500) {
  return { ok: true, value: { id, heartbeatIntervalMs, ttlMs: 5000 } } as const;
}

function granted(objectId: string): LockResult<Acquisition> {
  return {
    ok: true,
    value: {
      outcome: "granted",
      objectId,
      holder: { name: "alice@example.com", mine: true },
    },
  };
}

function failure<T>(error: LockError): LockResult<T> {
  return { ok: false, error };
}

function renewal(
  renewed: string[],
  lost: { objectId: string; reason: string }[] = [],
): LockResult<Renewal> {
  return {
    ok: true,
    value: {
      renewed: renewed.map((objectId) => ({ objectId, expiresInMs: 5000 })),
      lost,
    },
  };
}

let operations: number;
function setup() {
  const fake = fakeClient();
  const onLost = vi.fn();
  operations = 0;
  const controller = new LockController({
    client: fake.client,
    newOperationId: () => `op-${String(++operations)}`,
    onLost,
  });
  return { ...fake, onLost, controller };
}

beforeEach(() => {
  vi.useFakeTimers();
});

afterEach(() => {
  vi.useRealTimers();
});

describe("LockController", () => {
  it("opens a session only on the first acquire and heartbeats at the server interval", async () => {
    const { client, queues, controller } = setup();
    expect(client.open).not.toHaveBeenCalled();

    queues.open.push(session("pls-1", 1500));
    queues.acquire.push(granted("obj-1"));
    expect(await controller.acquire("obj-1")).toEqual({ kind: "granted" });
    expect(client.acquire).toHaveBeenCalledWith("pls-1", "op-1", "obj-1");
    expect(controller.held()).toEqual(["obj-1"]);

    queues.heartbeat.push(renewal(["obj-1"]), renewal(["obj-1"]));
    await vi.advanceTimersByTimeAsync(1499);
    expect(client.heartbeat).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(1);
    expect(client.heartbeat).toHaveBeenCalledTimes(1);
    expect(client.heartbeat).toHaveBeenCalledWith("pls-1");
    await vi.advanceTimersByTimeAsync(1500);
    expect(client.heartbeat).toHaveBeenCalledTimes(2);
    expect(controller.held()).toEqual(["obj-1"]);
    controller.dispose();
  });

  it("shares one session open between concurrent acquires", async () => {
    const { client, queues, controller } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"), granted("obj-2"));
    await Promise.all([
      controller.acquire("obj-1"),
      controller.acquire("obj-2"),
    ]);
    expect(client.open).toHaveBeenCalledTimes(1);
    expect(controller.held()).toEqual(["obj-1", "obj-2"]);
    controller.dispose();
  });

  it("reports a lock lost on a heartbeat and closes the idle session", async () => {
    const { client, queues, controller, onLost } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"));
    await controller.acquire("obj-1");

    queues.heartbeat.push(
      renewal([], [{ objectId: "obj-1", reason: "expired" }]),
    );
    await vi.advanceTimersByTimeAsync(1500);
    expect(onLost).toHaveBeenCalledExactlyOnceWith("obj-1", "expired");
    expect(controller.held()).toEqual([]);
    expect(client.end).toHaveBeenCalledExactlyOnceWith("pls-1");
    await vi.advanceTimersByTimeAsync(10_000);
    expect(client.heartbeat).toHaveBeenCalledTimes(1);
  });

  it("drops every lock when its session is gone and never restores them", async () => {
    const { client, queues, controller, onLost } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"), granted("obj-2"));
    await controller.acquire("obj-1");
    await controller.acquire("obj-2");

    queues.heartbeat.push(failure("session_gone"));
    await vi.advanceTimersByTimeAsync(1500);
    expect(onLost.mock.calls).toEqual([
      ["obj-1", "disconnected"],
      ["obj-2", "disconnected"],
    ]);
    expect(controller.held()).toEqual([]);
    // The server already ended it; nothing to delete.
    expect(client.end).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(client.heartbeat).toHaveBeenCalledTimes(1);

    queues.open.push(session("pls-2"));
    queues.acquire.push(granted("obj-1"));
    expect(await controller.acquire("obj-1")).toEqual({ kind: "granted" });
    expect(client.open).toHaveBeenCalledTimes(2);
    // A new session is a new intent: a new operation ID, never a replay.
    expect(client.acquire).toHaveBeenLastCalledWith("pls-2", "op-3", "obj-1");
    expect(controller.held()).toEqual(["obj-1"]);
    controller.dispose();
  });

  it("drops every lock when an acquire finds the session gone", async () => {
    const { client, queues, controller, onLost } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"), failure("session_gone"));
    await controller.acquire("obj-1");

    expect(await controller.acquire("obj-2")).toEqual({
      kind: "failed",
      error: "session_gone",
    });
    expect(onLost).toHaveBeenCalledExactlyOnceWith("obj-1", "disconnected");
    expect(controller.held()).toEqual([]);

    queues.open.push(session("pls-2"));
    queues.acquire.push(granted("obj-2"));
    await controller.acquire("obj-2");
    expect(client.acquire).toHaveBeenLastCalledWith("pls-2", "op-3", "obj-2");
    controller.dispose();
  });

  it("drops every lock when access is denied", async () => {
    const { queues, controller, onLost } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"));
    await controller.acquire("obj-1");
    queues.heartbeat.push(failure("forbidden"));
    await vi.advanceTimersByTimeAsync(1500);
    expect(onLost).toHaveBeenCalledExactlyOnceWith("obj-1", "disconnected");
    expect(controller.held()).toEqual([]);
  });

  it("retries an unanswered acquire with the same operation ID", async () => {
    const { client, queues, controller } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(
      failure("network"),
      failure("network"),
      granted("obj-1"),
    );
    expect(await controller.acquire("obj-1")).toEqual({ kind: "granted" });
    expect(client.acquire.mock.calls).toEqual([
      ["pls-1", "op-1", "obj-1"],
      ["pls-1", "op-1", "obj-1"],
      ["pls-1", "op-1", "obj-1"],
    ]);
    controller.dispose();
  });

  it("names the holder of a denied lock and keeps nothing", async () => {
    const { client, queues, controller } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push({
      ok: true,
      value: {
        outcome: "denied",
        objectId: "obj-1",
        holder: { name: "bob@example.com", mine: false },
      },
    });
    expect(await controller.acquire("obj-1")).toEqual({
      kind: "denied",
      holder: { name: "bob@example.com", mine: false },
    });
    expect(controller.held()).toEqual([]);
    expect(client.end).toHaveBeenCalledExactlyOnceWith("pls-1");
  });

  for (const reason of ["finished", "cancelled"] as const) {
    it(`releases a ${reason} lock at once`, async () => {
      const { client, queues, controller } = setup();
      queues.open.push(session("pls-1"));
      queues.acquire.push(granted("obj-1"));
      await controller.acquire("obj-1");

      let answer!: (result: LockResult<undefined>) => void;
      queues.release.push(new Promise((resolve) => (answer = resolve)));
      const released =
        reason === "finished"
          ? controller.finish("obj-1")
          : controller.cancel("obj-1");
      expect(controller.held()).toEqual([]);
      await vi.advanceTimersByTimeAsync(0);
      expect(client.release).toHaveBeenCalledExactlyOnceWith(
        "pls-1",
        "op-2",
        "obj-1",
        reason,
      );
      expect(client.end).not.toHaveBeenCalled();
      answer({ ok: true, value: undefined });
      expect(await released).toEqual({ kind: "released" });
      expect(client.end).toHaveBeenCalledExactlyOnceWith("pls-1");
    });
  }

  it("does not release a lock it does not hold", async () => {
    const { client, controller } = setup();
    expect(await controller.finish("obj-1")).toEqual({
      kind: "failed",
      error: "not_holder",
    });
    expect(client.release).not.toHaveBeenCalled();
  });

  it("stops heartbeating and ends its session when disposed", async () => {
    const { client, queues, controller, onLost } = setup();
    queues.open.push(session("pls-1"));
    queues.acquire.push(granted("obj-1"));
    await controller.acquire("obj-1");

    controller.dispose();
    expect(client.end).toHaveBeenCalledExactlyOnceWith("pls-1");
    expect(controller.held()).toEqual([]);
    expect(onLost).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(10_000);
    expect(client.heartbeat).not.toHaveBeenCalled();
    expect(await controller.acquire("obj-2")).toEqual({
      kind: "failed",
      error: "unavailable",
    });
    expect(client.open).toHaveBeenCalledTimes(1);
  });

  it("ends a session that opens after it was disposed", async () => {
    const { client, queues, controller } = setup();
    let answer!: (result: LockResult<LockSessionInfo>) => void;
    queues.open.push(new Promise((resolve) => (answer = resolve)));
    const acquired = controller.acquire("obj-1");
    controller.dispose();
    answer(session("pls-1"));
    expect(await acquired).toEqual({ kind: "failed", error: "unavailable" });
    expect(client.acquire).not.toHaveBeenCalled();
    expect(client.end).toHaveBeenCalledExactlyOnceWith("pls-1");
  });
});
