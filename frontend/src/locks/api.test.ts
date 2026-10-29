import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import {
  acquireLock,
  endLockSession,
  heartbeatLocks,
  listLocks,
  openLockSession,
  releaseLock,
} from "./api";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const canvas = { tenantId: "tenant-a", canvasId: "canvas-1" };
const base = "/api/tenants/tenant-a/canvases/canvas-1/locks";
const sessionId = `pls_${"a".repeat(32)}`;

describe("lock requests", () => {
  it("opens a lock session with the CSRF token", async () => {
    replies.push(
      json(201, {
        lock_session_id: sessionId,
        heartbeat_interval_ms: 2000,
        ttl_ms: 5000,
      }),
    );
    const result = await openLockSession("csrf-1", canvas);
    expect(result).toEqual({
      ok: true,
      value: { id: sessionId, heartbeatIntervalMs: 2000, ttlMs: 5000 },
    });
    expect(calls).toEqual([
      {
        method: "POST",
        path: `${base}/sessions`,
        headers: { "X-CSRF-Token": "csrf-1" },
        body: undefined,
        raw: undefined,
      },
    ]);
  });

  it("renews and reports lost locks on a heartbeat", async () => {
    replies.push(
      json(200, {
        renewed: [{ object_id: "obj-1", expires_in_ms: 5000 }],
        lost: [{ object_id: "obj-2", reason: "expired" }],
      }),
    );
    const result = await heartbeatLocks("csrf-1", canvas, sessionId);
    expect(result).toEqual({
      ok: true,
      value: {
        renewed: [{ objectId: "obj-1", expiresInMs: 5000 }],
        lost: [{ objectId: "obj-2", reason: "expired" }],
      },
    });
    expect(calls[0]?.method).toBe("POST");
    expect(calls[0]?.path).toBe(`${base}/sessions/${sessionId}/heartbeat`);
    expect(calls[0]?.headers).toEqual({ "X-CSRF-Token": "csrf-1" });
  });

  it("sends an acquire and names the holder of a granted or denied lock", async () => {
    replies.push(
      json(200, {
        operation_id: "op-1",
        outcome: "granted",
        object_id: "obj-1",
        holder: { name: "alice@example.com", mine: true },
        expires_in_ms: 5000,
      }),
      json(200, {
        operation_id: "op-2",
        outcome: "denied",
        object_id: "obj-1",
        holder: { name: "bob@example.com", mine: false },
        expires_in_ms: 3000,
      }),
    );
    expect(
      await acquireLock("csrf-1", canvas, sessionId, "op-1", "obj-1"),
    ).toEqual({
      ok: true,
      value: {
        outcome: "granted",
        objectId: "obj-1",
        holder: { name: "alice@example.com", mine: true },
      },
    });
    expect(
      await acquireLock("csrf-1", canvas, sessionId, "op-2", "obj-1"),
    ).toEqual({
      ok: true,
      value: {
        outcome: "denied",
        objectId: "obj-1",
        holder: { name: "bob@example.com", mine: false },
      },
    });
    expect(calls[0]).toEqual({
      method: "POST",
      path: `${base}/sessions/${sessionId}/acquire`,
      headers: { "X-CSRF-Token": "csrf-1", "Content-Type": "application/json" },
      body: { operation_id: "op-1", object_id: "obj-1" },
      raw: undefined,
    });
  });

  it("sends a release with its reason", async () => {
    replies.push(
      json(200, {
        operation_id: "op-3",
        outcome: "released",
        reason: "cancelled",
        object_id: "obj-1",
      }),
    );
    expect(
      await releaseLock(
        "csrf-1",
        canvas,
        sessionId,
        "op-3",
        "obj-1",
        "cancelled",
      ),
    ).toEqual({ ok: true, value: undefined });
    expect(calls[0]?.path).toBe(`${base}/sessions/${sessionId}/release`);
    expect(calls[0]?.body).toEqual({
      operation_id: "op-3",
      object_id: "obj-1",
      reason: "cancelled",
    });
  });

  it("ends a lock session with DELETE", async () => {
    replies.push(json(204));
    expect(await endLockSession("csrf-1", canvas, sessionId)).toEqual({
      ok: true,
      value: undefined,
    });
    expect(calls[0]?.method).toBe("DELETE");
    expect(calls[0]?.path).toBe(`${base}/sessions/${sessionId}`);
    expect(calls[0]?.headers).toEqual({ "X-CSRF-Token": "csrf-1" });
  });

  it("lists holders by name only", async () => {
    replies.push(
      json(200, {
        locks: [
          {
            object_id: "obj-1",
            holder: {
              name: "bob@example.com",
              mine: false,
              account_id: "decoy-account",
            },
            lock_session_id: "decoy-session",
          },
        ],
      }),
    );
    expect(await listLocks(canvas)).toEqual({
      ok: true,
      value: [
        { objectId: "obj-1", holder: { name: "bob@example.com", mine: false } },
      ],
    });
    expect(calls[0]?.method).toBe("GET");
    expect(calls[0]?.path).toBe(base);
  });

  it("encodes path segments", async () => {
    replies.push(json(200, { locks: [] }));
    await listLocks({ tenantId: "a/b", canvasId: "c d" });
    expect(calls[0]?.path).toBe("/api/tenants/a%2Fb/canvases/c%20d/locks");
  });
});

describe("lock answers", () => {
  const cases: [Reply, string][] = [
    [json(410, { error: "lock_session_gone" }), "session_gone"],
    [json(409, { error: "not_lock_holder" }), "not_holder"],
    [json(409, { error: "conflict" }), "conflict"],
    [json(403, { error: "forbidden" }), "forbidden"],
    [json(401, { error: "unauthorized" }), "unauthorized"],
    [json(400, { error: "bad_request" }), "bad_request"],
    [json(429, { error: "rate_limited" }), "rate_limited"],
    [json(503, { error: "lock_unavailable" }), "unavailable"],
    [json(500, { error: "something the server said" }), "unavailable"],
    ["network-error", "network"],
  ];
  for (const [reply, error] of cases) {
    it(`maps ${JSON.stringify(reply)} to ${error}`, async () => {
      replies.push(reply);
      expect(
        await acquireLock("csrf-1", canvas, sessionId, "op-1", "obj-1"),
      ).toEqual({ ok: false, error });
    });
  }

  it("rejects malformed success bodies", async () => {
    replies.push(
      json(200, { outcome: "maybe", object_id: "obj-1", holder: {} }),
      json(201, { lock_session_id: 7 }),
      json(200, { locks: [{ object_id: "obj-1" }] }),
      json(200, { renewed: "x", lost: [] }),
    );
    expect(
      await acquireLock("csrf-1", canvas, sessionId, "op-1", "obj-1"),
    ).toEqual({ ok: false, error: "unavailable" });
    expect(await openLockSession("csrf-1", canvas)).toEqual({
      ok: false,
      error: "unavailable",
    });
    expect(await listLocks(canvas)).toEqual({
      ok: false,
      error: "unavailable",
    });
    expect(await heartbeatLocks("csrf-1", canvas, sessionId)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});
