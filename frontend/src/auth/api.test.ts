import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { activate, deactivate, getOwnCanvas, getSession } from "./api";

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

describe("getOwnCanvas", () => {
  it("reads the own canvas", async () => {
    replies.push(
      json(200, {
        tenant_id: tenantId,
        canvas_id: canvasId,
        width: 1920,
        height: 1080,
        revision: 0,
        objects: [],
      }),
    );

    expect(await getOwnCanvas()).toEqual({
      ok: true,
      value: { tenantId, canvasId, width: 1920, height: 1080 },
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
