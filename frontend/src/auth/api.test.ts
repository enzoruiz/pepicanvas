import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import {
  activate,
  deactivate,
  getOwnCanvas,
  getSession,
  setDimensions,
} from "./api";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  vi.unstubAllGlobals();
});

const tenantId = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const canvasId = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";
const accountId = "0123456789abcdef0123456789abcdef";

describe("getSession", () => {
  it.each([true, false])("reads operator %s", async (operator) => {
    replies.push(
      json(200, {
        account_id: accountId,
        email: "alice@example.com",
        csrf_token: "csrf",
        operator,
      }),
    );

    expect(await getSession()).toEqual({
      ok: true,
      value: {
        accountId,
        email: "alice@example.com",
        csrfToken: "csrf",
        operator,
      },
    });
  });

  it.each([undefined, "true", 1])(
    "rejects a session whose operator flag is %s",
    async (operator) => {
      replies.push(
        json(200, {
          account_id: accountId,
          email: "alice@example.com",
          csrf_token: "csrf",
          operator,
        }),
      );

      expect(await getSession()).toEqual({ ok: false, error: "unavailable" });
    },
  );
});

const textObject = {
  id: "obj-1",
  kind: "text",
  content: "Hello",
  x: -10,
  y: 20,
  width: 300,
  height: 40,
  rotation: 12.5,
  z: 3,
};

describe("getOwnCanvas", () => {
  it("reads the own canvas", async () => {
    replies.push(
      json(200, {
        tenant_id: tenantId,
        canvas_id: canvasId,
        width: 1920,
        height: 1080,
        revision: 7,
        objects: [textObject],
      }),
    );

    expect(await getOwnCanvas()).toEqual({
      ok: true,
      value: {
        tenantId,
        canvasId,
        width: 1920,
        height: 1080,
        revision: 7,
        objects: [
          {
            id: "obj-1",
            kind: "text",
            content: "Hello",
            x: -10,
            y: 20,
            width: 300,
            height: 40,
            rotation: 12.5,
            z: 3,
          },
        ],
      },
    });
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/canvas" });
  });

  it("reports forbidden for an account that is not active", async () => {
    replies.push(json(403, { error: "forbidden" }));

    expect(await getOwnCanvas()).toEqual({ ok: false, error: "forbidden" });
  });

  it("knows subscription_inactive for a deactivated owner", async () => {
    replies.push(json(403, { error: "subscription_inactive" }));

    expect(await getOwnCanvas()).toEqual({
      ok: false,
      error: "subscription_inactive",
    });
  });

  it("rejects a malformed canvas body", async () => {
    replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));

    expect(await getOwnCanvas()).toEqual({ ok: false, error: "unavailable" });
  });

  it.each([
    ["no revision", { revision: undefined }],
    ["a fractional revision", { revision: 1.5 }],
    ["no objects", { objects: undefined }],
    ["objects that are not a list", { objects: {} }],
    ["a null object", { objects: [null] }],
    ["an object without id", { objects: [{ ...textObject, id: 1 }] }],
    ["an object without kind", { objects: [{ ...textObject, kind: null }] }],
    ["an object without content", { objects: [{ ...textObject, content: 0 }] }],
    ["a fractional x", { objects: [{ ...textObject, x: 1.5 }] }],
    ["a string y", { objects: [{ ...textObject, y: "20" }] }],
    ["a missing width", { objects: [{ ...textObject, width: undefined }] }],
    ["a fractional height", { objects: [{ ...textObject, height: 0.5 }] }],
    ["a string rotation", { objects: [{ ...textObject, rotation: "12" }] }],
    ["a fractional z", { objects: [{ ...textObject, z: 0.5 }] }],
    ["a fractional canvas width", { width: 1920.5 }],
  ])("rejects a canvas with %s", async (_, change) => {
    replies.push(
      json(200, {
        tenant_id: tenantId,
        canvas_id: canvasId,
        width: 1920,
        height: 1080,
        revision: 0,
        objects: [textObject],
        ...change,
      }),
    );

    expect(await getOwnCanvas()).toEqual({ ok: false, error: "unavailable" });
  });
});

describe("setDimensions", () => {
  it("sends the operation, size, and CSRF token to the canvas", async () => {
    replies.push(
      json(200, { canvas_id: canvasId, width: 320, height: 180, revision: 4 }),
    );

    expect(
      await setDimensions(
        "csrf",
        tenantId,
        canvasId,
        "operation-id-0001",
        320,
        180,
      ),
    ).toEqual({
      ok: true,
      value: { canvasId, width: 320, height: 180, revision: 4 },
    });
    expect(calls[0]).toMatchObject({
      method: "PUT",
      path: `/api/tenants/${tenantId}/canvases/${canvasId}/dimensions`,
      headers: { "X-CSRF-Token": "csrf", "Content-Type": "application/json" },
      body: { operation_id: "operation-id-0001", width: 320, height: 180 },
    });
    expect(Object.keys(calls[0].body ?? {}).sort()).toEqual([
      "height",
      "operation_id",
      "width",
    ]);
  });

  it.each([
    ["bad_request", 400],
    ["forbidden", 403],
    ["conflict", 409],
    ["unauthorized", 401],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code, correlation_id: "c" }));

    expect(
      await setDimensions("csrf", tenantId, canvasId, "op", 320, 180),
    ).toEqual({ ok: false, error: code });
  });

  it.each([json(500, { error: "internal" }), "network-error" as const])(
    "reports %o as unavailable",
    async (reply) => {
      replies.push(reply);

      expect(
        await setDimensions("csrf", tenantId, canvasId, "op", 320, 180),
      ).toEqual({ ok: false, error: "unavailable" });
    },
  );

  it.each([
    { width: 320, height: 180, revision: 4 },
    { canvas_id: canvasId, width: "320", height: 180, revision: 4 },
    { canvas_id: canvasId, width: 320, height: 180.5, revision: 4 },
    { canvas_id: canvasId, width: 320, height: 180 },
  ])("rejects a malformed answer %o", async (body) => {
    replies.push(json(200, body));

    expect(
      await setDimensions("csrf", tenantId, canvasId, "op", 320, 180),
    ).toEqual({ ok: false, error: "unavailable" });
  });
});

describe("activate", () => {
  it("sends the operation, email, and CSRF token", async () => {
    replies.push(
      json(200, {
        account_id: accountId,
        tenant_id: tenantId,
        canvas_id: canvasId,
        created: true,
      }),
    );

    expect(
      await activate("csrf", "operation-id-0001", "bob@example.com"),
    ).toEqual({
      ok: true,
      value: { accountId, tenantId, canvasId, created: true },
    });
    expect(calls[0]).toMatchObject({
      method: "POST",
      path: "/api/activations",
      headers: { "X-CSRF-Token": "csrf" },
      body: { operation_id: "operation-id-0001", email: "bob@example.com" },
    });
  });

  it("knows not_found", async () => {
    replies.push(json(404, { error: "not_found" }));

    expect(await activate("csrf", "operation-id-0001", "x@y.z")).toEqual({
      ok: false,
      error: "not_found",
    });
  });

  it("rejects an answer without the created flag", async () => {
    replies.push(
      json(200, {
        account_id: accountId,
        tenant_id: tenantId,
        canvas_id: canvasId,
      }),
    );

    expect(await activate("csrf", "operation-id-0001", "x@y.z")).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});

describe("deactivate", () => {
  it.each([true, false])(
    "sends the operation, email, and CSRF token and reads deactivated %s",
    async (deactivated) => {
      replies.push(
        json(200, {
          account_id: accountId,
          tenant_id: tenantId,
          canvas_id: canvasId,
          deactivated,
        }),
      );

      expect(
        await deactivate("csrf", "operation-id-0001", "bob@example.com"),
      ).toEqual({
        ok: true,
        value: { accountId, tenantId, canvasId, deactivated },
      });
      expect(calls[0]).toMatchObject({
        method: "POST",
        path: "/api/deactivations",
        headers: { "X-CSRF-Token": "csrf" },
        body: { operation_id: "operation-id-0001", email: "bob@example.com" },
      });
    },
  );

  it.each([
    ["bad_request", 400],
    ["forbidden", 403],
    ["not_found", 404],
    ["conflict", 409],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code, detail: "ignored" }));

    expect(await deactivate("csrf", "operation-id-0001", "x@y.z")).toEqual({
      ok: false,
      error: code,
    });
  });

  it.each([json(500, { error: "internal" }), "network-error" as const])(
    "reports unavailable for %o",
    async (reply) => {
      replies.push(reply);

      expect(await deactivate("csrf", "operation-id-0001", "x@y.z")).toEqual({
        ok: false,
        error: "unavailable",
      });
    },
  );

  it.each([
    { account_id: accountId, tenant_id: tenantId, canvas_id: canvasId },
    {
      account_id: accountId,
      tenant_id: tenantId,
      canvas_id: canvasId,
      deactivated: "true",
    },
    { tenant_id: tenantId, canvas_id: canvasId, deactivated: true },
    {
      account_id: accountId,
      tenant_id: tenantId,
      canvas_id: canvasId,
      created: true,
    },
  ])("rejects a malformed answer %o", async (body) => {
    replies.push(json(200, body));

    expect(await deactivate("csrf", "operation-id-0001", "x@y.z")).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});
