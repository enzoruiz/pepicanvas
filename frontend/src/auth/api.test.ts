import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import {
  acceptInvitation,
  activate,
  createInvitation,
  deactivate,
  generateInvitationToken,
  getMediaUsage,
  getOwnCanvas,
  getSession,
  getTenantCanvas,
  listInvitations,
  listMembers,
  listMemberships,
  revokeInvitation,
  revokeMember,
  setDimensions,
  uploadMedia,
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
    ["no tenant", { tenant_id: undefined }],
    ["a numeric tenant", { tenant_id: 1 }],
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

const invitationId = "cccccccccccccccccccccccccccccccc";
const token = `pci_${"A".repeat(43)}`;
const createdAt = "2026-10-22T10:00:00.123456Z";
const expiresAt = "2026-10-29T10:00:00.123456Z";

describe("generateInvitationToken", () => {
  it("draws 32 random bytes into a pci_ base64url token", () => {
    const spy = vi.spyOn(crypto, "getRandomValues");
    const generated = generateInvitationToken();

    expect(generated).toMatch(/^pci_[A-Za-z0-9_-]{43}$/);
    expect(spy).toHaveBeenCalledTimes(1);
    expect((spy.mock.calls[0][0] as Uint8Array).byteLength).toBe(32);
    spy.mockRestore();
  });

  it("encodes the bytes without padding", () => {
    const spy = vi
      .spyOn(crypto, "getRandomValues")
      .mockImplementation(<T extends ArrayBufferView | null>(array: T): T => {
        (array as unknown as Uint8Array).fill(0xff);
        return array;
      });

    expect(generateInvitationToken()).toBe(`pci_${"_".repeat(42)}8`);
    spy.mockRestore();
  });

  it("never repeats a token", () => {
    const tokens = new Set(
      Array.from({ length: 200 }, () => generateInvitationToken()),
    );

    expect(tokens.size).toBe(200);
  });
});

describe("createInvitation", () => {
  it("sends the token only in the JSON body with the CSRF token", async () => {
    replies.push(
      json(201, {
        invitation_id: invitationId,
        created_at: createdAt,
        expires_at: expiresAt,
      }),
    );

    expect(await createInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: true,
      value: { invitationId, createdAt, expiresAt },
    });
    expect(calls[0]).toEqual({
      method: "POST",
      path: "/api/invitations",
      headers: { "X-CSRF-Token": "csrf", "Content-Type": "application/json" },
      body: { operation_id: "operation-id-0001", token },
    });
  });

  it.each([
    { created_at: createdAt, expires_at: expiresAt },
    { invitation_id: "nope", created_at: createdAt, expires_at: expiresAt },
    { invitation_id: invitationId, created_at: createdAt },
    { invitation_id: invitationId, created_at: 1, expires_at: expiresAt },
    { invitation_id: invitationId, created_at: createdAt, expires_at: "soon" },
  ])("rejects the malformed answer %o", async (body) => {
    replies.push(json(201, body));

    expect(await createInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it.each([
    ["rate_limited", 429],
    ["forbidden", 403],
    ["conflict", 409],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code }));

    expect(await createInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: code,
    });
  });

  it("reports a network failure as unavailable", async () => {
    replies.push("network-error");

    expect(await createInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});

describe("listInvitations", () => {
  it("reads the pending invitations without a token", async () => {
    replies.push(
      json(200, {
        invitations: [
          {
            invitation_id: invitationId,
            created_at: createdAt,
            expires_at: expiresAt,
          },
        ],
      }),
    );

    expect(await listInvitations()).toEqual({
      ok: true,
      value: [{ invitationId, createdAt, expiresAt }],
    });
    expect(calls[0]).toMatchObject({
      method: "GET",
      path: "/api/invitations",
      body: undefined,
    });
  });

  it("reads an empty list", async () => {
    replies.push(json(200, { invitations: [] }));

    expect(await listInvitations()).toEqual({ ok: true, value: [] });
  });

  it.each([
    {},
    { invitations: null },
    { invitations: [{ invitation_id: invitationId, created_at: createdAt }] },
  ])("rejects the malformed list %o", async (body) => {
    replies.push(json(200, body));

    expect(await listInvitations()).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});

describe("revokeInvitation", () => {
  it.each(["revoked", "accepted"])(
    "sends the operation with the CSRF token and reads %s",
    async (status) => {
      replies.push(json(200, { invitation_id: invitationId, status }));

      expect(
        await revokeInvitation("csrf", "operation-id-0001", invitationId),
      ).toEqual({ ok: true, value: { invitationId, status } });
      expect(calls[0]).toEqual({
        method: "POST",
        path: `/api/invitations/${invitationId}/revoke`,
        headers: {
          "X-CSRF-Token": "csrf",
          "Content-Type": "application/json",
        },
        body: { operation_id: "operation-id-0001" },
      });
    },
  );

  it.each([
    { invitation_id: invitationId, status: "pending" },
    { invitation_id: invitationId },
    { status: "revoked" },
  ])("rejects the malformed answer %o", async (body) => {
    replies.push(json(200, body));

    expect(
      await revokeInvitation("csrf", "operation-id-0001", invitationId),
    ).toEqual({ ok: false, error: "unavailable" });
  });

  it("knows not_found", async () => {
    replies.push(json(404, { error: "not_found" }));

    expect(
      await revokeInvitation("csrf", "operation-id-0001", invitationId),
    ).toEqual({ ok: false, error: "not_found" });
  });
});

describe("acceptInvitation", () => {
  it("sends the token only in the JSON body with the CSRF token", async () => {
    replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));

    expect(await acceptInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: true,
      value: { tenantId, canvasId },
    });
    expect(calls).toEqual([
      {
        method: "POST",
        path: "/api/invitations/accept",
        headers: { "X-CSRF-Token": "csrf", "Content-Type": "application/json" },
        body: { operation_id: "operation-id-0001", token },
      },
    ]);
  });

  it.each([
    {},
    { tenant_id: tenantId },
    { canvas_id: canvasId },
    { tenant_id: "../x", canvas_id: canvasId },
    { tenant_id: tenantId, canvas_id: 7 },
  ])("rejects the malformed answer %o", async (body) => {
    replies.push(json(200, body));

    expect(await acceptInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it.each([
    ["not_found", 404],
    ["bad_request", 400],
    ["conflict", 409],
    ["rate_limited", 429],
    ["unauthorized", 401],
    ["forbidden", 403],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code }));

    expect(await acceptInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: code,
    });
  });

  it.each([
    ["a network failure", "network-error" as const],
    ["an internal error", json(500, { error: "internal" })],
  ])("reports %s as unavailable", async (_, reply) => {
    replies.push(reply);

    expect(await acceptInvitation("csrf", "operation-id-0001", token)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});

describe("listMemberships", () => {
  it("reads the moderator memberships", async () => {
    const otherTenant = "dddddddddddddddddddddddddddddddd";
    const otherCanvas = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee";
    replies.push(
      json(200, {
        memberships: [
          { tenant_id: tenantId, canvas_id: canvasId, role: "moderator" },
          { tenant_id: otherTenant, canvas_id: otherCanvas, role: "moderator" },
        ],
      }),
    );

    expect(await listMemberships()).toEqual({
      ok: true,
      value: [
        { tenantId, canvasId, role: "moderator" },
        { tenantId: otherTenant, canvasId: otherCanvas, role: "moderator" },
      ],
    });
    expect(calls).toEqual([
      { method: "GET", path: "/api/memberships", headers: {}, body: undefined },
    ]);
  });

  it("reads an empty list", async () => {
    replies.push(json(200, { memberships: [] }));

    expect(await listMemberships()).toEqual({ ok: true, value: [] });
  });

  it.each([
    ["no list", {}],
    ["a list that is not an array", { memberships: {} }],
    ["a null entry", { memberships: [null] }],
    [
      "an owner role",
      {
        memberships: [
          { tenant_id: tenantId, canvas_id: canvasId, role: "owner" },
        ],
      },
    ],
    [
      "a malformed tenant",
      {
        memberships: [
          { tenant_id: "a/b", canvas_id: canvasId, role: "moderator" },
        ],
      },
    ],
    [
      "a missing canvas",
      { memberships: [{ tenant_id: tenantId, role: "moderator" }] },
    ],
  ])("rejects an answer with %s", async (_, body) => {
    replies.push(json(200, body));

    expect(await listMemberships()).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it("reports unauthorized", async () => {
    replies.push(json(401, { error: "unauthorized" }));

    expect(await listMemberships()).toEqual({
      ok: false,
      error: "unauthorized",
    });
  });
});

describe("listMembers", () => {
  const otherAccount = "fedcba9876543210fedcba9876543210";

  it("reads the moderators with their email and acceptance date", async () => {
    replies.push(
      json(200, {
        members: [
          {
            account_id: accountId,
            email: "bob@example.com",
            accepted_at: "2026-10-28T10:00:00Z",
          },
          {
            account_id: otherAccount,
            email: "carol@example.com",
            accepted_at: null,
          },
        ],
      }),
    );

    expect(await listMembers()).toEqual({
      ok: true,
      value: [
        {
          accountId,
          email: "bob@example.com",
          acceptedAt: new Date("2026-10-28T10:00:00Z"),
        },
        {
          accountId: otherAccount,
          email: "carol@example.com",
          acceptedAt: null,
        },
      ],
    });
    expect(calls).toEqual([
      { method: "GET", path: "/api/members", headers: {}, body: undefined },
    ]);
  });

  it("reads an empty list", async () => {
    replies.push(json(200, { members: [] }));

    expect(await listMembers()).toEqual({ ok: true, value: [] });
  });

  const member = {
    account_id: accountId,
    email: "bob@example.com",
    accepted_at: "2026-10-28T10:00:00Z",
  };

  it.each([
    ["no list", {}],
    ["a list that is not an array", { members: {} }],
    ["a null entry", { members: [null] }],
    ["a short account", { members: [{ ...member, account_id: "abc" }] }],
    [
      "an uppercase account",
      { members: [{ ...member, account_id: accountId.toUpperCase() }] },
    ],
    ["a path account", { members: [{ ...member, account_id: "../members" }] }],
    ["a missing email", { members: [{ ...member, email: undefined }] }],
    ["a numeric email", { members: [{ ...member, email: 7 }] }],
    ["a missing date", { members: [{ ...member, accepted_at: undefined }] }],
    [
      "a date-only value",
      { members: [{ ...member, accepted_at: "2026-10-28" }] },
    ],
    ["a non-date", { members: [{ ...member, accepted_at: "yesterday" }] }],
    ["a numeric date", { members: [{ ...member, accepted_at: 1761645600 }] }],
    [
      "an impossible date",
      { members: [{ ...member, accepted_at: "2026-13-40T10:00:00Z" }] },
    ],
  ])("rejects an answer with %s", async (_, body) => {
    replies.push(json(200, body));

    expect(await listMembers()).toEqual({ ok: false, error: "unavailable" });
  });

  it("accepts a fractional second and an offset", async () => {
    replies.push(
      json(200, {
        members: [{ ...member, accepted_at: "2026-10-28T12:00:00.5+02:00" }],
      }),
    );

    expect(await listMembers()).toEqual({
      ok: true,
      value: [
        {
          accountId,
          email: "bob@example.com",
          acceptedAt: new Date("2026-10-28T10:00:00.5Z"),
        },
      ],
    });
  });

  it.each([
    ["forbidden", 403],
    ["unauthorized", 401],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code }));

    expect(await listMembers()).toEqual({ ok: false, error: code });
  });
});

describe("revokeMember", () => {
  it("sends the operation with the CSRF token to the member's route", async () => {
    replies.push(json(200, { tenant_id: tenantId, account_id: accountId }));

    expect(await revokeMember("csrf", "operation-id-0001", accountId)).toEqual({
      ok: true,
      value: { tenantId, accountId },
    });
    expect(calls).toEqual([
      {
        method: "POST",
        path: `/api/members/${accountId}/revoke`,
        headers: { "X-CSRF-Token": "csrf", "Content-Type": "application/json" },
        body: { operation_id: "operation-id-0001" },
      },
    ]);
  });

  it("refuses a malformed account before any request", async () => {
    expect(
      await revokeMember("csrf", "operation-id-0001", "../invitations"),
    ).toEqual({ ok: false, error: "bad_request" });
    expect(calls).toEqual([]);
  });

  it.each([
    ["no body", undefined],
    ["no tenant", { account_id: accountId }],
    ["a malformed tenant", { tenant_id: "a/b", account_id: accountId }],
    [
      "another account",
      { tenant_id: tenantId, account_id: "fedcba9876543210fedcba9876543210" },
    ],
  ])("rejects an answer with %s", async (_, body) => {
    replies.push(json(200, body));

    expect(await revokeMember("csrf", "operation-id-0001", accountId)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it.each([
    ["bad_request", 400],
    ["unauthorized", 401],
    ["forbidden", 403],
    ["not_found", 404],
    ["conflict", 409],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code, detail: "secret" }));

    expect(await revokeMember("csrf", "operation-id-0001", accountId)).toEqual({
      ok: false,
      error: code,
    });
  });

  it.each([
    ["a network failure", "network-error" as const],
    ["an internal error", json(500, { error: "internal" })],
  ])("reports %s as unavailable", async (_, reply) => {
    replies.push(reply);

    expect(await revokeMember("csrf", "operation-id-0001", accountId)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });
});

describe("getTenantCanvas", () => {
  const body = {
    canvas_id: canvasId,
    width: 1280,
    height: 720,
    revision: 4,
    objects: [textObject],
  };

  it("reads a canvas through the tenant route", async () => {
    replies.push(json(200, body));

    expect(await getTenantCanvas(tenantId, canvasId)).toEqual({
      ok: true,
      value: {
        tenantId,
        canvasId,
        width: 1280,
        height: 720,
        revision: 4,
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
    expect(calls).toEqual([
      {
        method: "GET",
        path: `/api/tenants/${tenantId}/canvases/${canvasId}`,
        headers: {},
        body: undefined,
      },
    ]);
  });

  it.each([
    ["another canvas", { canvas_id: "ffffffffffffffffffffffffffffffff" }],
    ["no revision", { revision: undefined }],
    ["a malformed object", { objects: [{ ...textObject, z: 0.5 }] }],
  ])("rejects an answer for %s", async (_, change) => {
    replies.push(json(200, { ...body, ...change }));

    expect(await getTenantCanvas(tenantId, canvasId)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it("reports forbidden for a canvas the account may not view", async () => {
    replies.push(json(403, { error: "forbidden" }));

    expect(await getTenantCanvas(tenantId, canvasId)).toEqual({
      ok: false,
      error: "forbidden",
    });
  });
});

const usagePath = `/api/tenants/${tenantId}/canvases/${canvasId}/media/usage`;

const usageBody = {
  quota_bytes: 1_000_000_000,
  used_bytes: 250,
  reserved_bytes: 40,
  available_bytes: 999_999_710,
};

const usage = {
  quotaBytes: 1_000_000_000,
  usedBytes: 250,
  reservedBytes: 40,
  availableBytes: 999_999_710,
};

describe("getMediaUsage", () => {
  it("reads the aggregate usage of the canvas", async () => {
    replies.push(json(200, usageBody));

    expect(await getMediaUsage(tenantId, canvasId)).toEqual({
      ok: true,
      value: usage,
    });
    expect(calls[0]).toMatchObject({ method: "GET", path: usagePath });
  });

  it("encodes the tenant and canvas IDs in the path", async () => {
    replies.push(json(200, usageBody));

    await getMediaUsage("a/b", "c?d");

    expect(calls[0].path).toBe("/api/tenants/a%2Fb/canvases/c%3Fd/media/usage");
  });

  it.each([
    ["no quota", { quota_bytes: undefined }],
    ["a fractional used count", { used_bytes: 1.5 }],
    ["a negative reserved count", { reserved_bytes: -1 }],
    ["a text available count", { available_bytes: "1" }],
  ])("rejects an answer with %s", async (_, change) => {
    replies.push(json(200, { ...usageBody, ...change }));

    expect(await getMediaUsage(tenantId, canvasId)).toEqual({
      ok: false,
      error: "unavailable",
    });
  });

  it.each(["forbidden", "media_unavailable"])("knows %s", async (code) => {
    replies.push(json(code === "forbidden" ? 403 : 503, { error: code }));

    expect(await getMediaUsage(tenantId, canvasId)).toEqual({
      ok: false,
      error: code,
    });
  });
});

describe("uploadMedia", () => {
  const file = new File(["12345"], "cat.png", { type: "image/png" });
  const uploaded = {
    asset_id: "dddddddddddddddddddddddddddddddd",
    kind: "image",
    usage: usageBody,
  };

  it("sends the raw file with the CSRF token and the IDs in the query", async () => {
    replies.push(json(201, uploaded));

    expect(
      await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op+1/x",
        referenceId: "ref&2=y",
        file,
      }),
    ).toEqual({
      ok: true,
      value: {
        assetId: "dddddddddddddddddddddddddddddddd",
        kind: "image",
        usage,
      },
    });
    expect(calls[0]).toEqual({
      method: "POST",
      path: `/api/tenants/${tenantId}/canvases/${canvasId}/media/uploads?operation_id=op%2B1%2Fx&reference_id=ref%262%3Dy`,
      headers: {
        "X-CSRF-Token": "csrf",
        "Content-Type": "application/octet-stream",
      },
      body: undefined,
      raw: file,
    });
  });

  it("asks for a replacement with replace=true", async () => {
    replies.push(json(201, uploaded));

    await uploadMedia("csrf", tenantId, canvasId, {
      operationId: "op-1",
      referenceId: "ref-1",
      file,
      replace: true,
    });

    expect(calls[0].path).toBe(
      `/api/tenants/${tenantId}/canvases/${canvasId}/media/uploads?operation_id=op-1&reference_id=ref-1&replace=true`,
    );
  });

  it.each([
    ["quota_exceeded", 413],
    ["upload_in_progress", 409],
    ["upload_failed", 502],
    ["reference_exists", 409],
    ["length_required", 411],
    ["media_unavailable", 503],
    ["forbidden", 403],
    ["rate_limited", 429],
  ])("knows %s", async (code, status) => {
    replies.push(json(status, { error: code, correlation_id: "corr-1" }));

    expect(
      await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op-1",
        referenceId: "ref-1",
        file,
      }),
    ).toEqual({ ok: false, error: code });
  });

  it.each([
    "invalid_limits",
    "unsupported_kind",
    "invalid_metadata",
    "input_too_large",
    "dimensions_exceeded",
    "duration_exceeded",
    "frame_count_exceeded",
    "stream_count_exceeded",
  ])("reports the rejection reason %s", async (reason) => {
    replies.push(json(422, { error: "media_rejected", reason }));

    expect(
      await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op-1",
        referenceId: "ref-1",
        file,
      }),
    ).toEqual({ ok: false, error: "media_rejected", reason });
  });

  it.each([undefined, "something_new", "<b>x</b>", 7])(
    "drops the unknown rejection reason %o",
    async (reason) => {
      replies.push(json(422, { error: "media_rejected", reason }));

      const result = await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op-1",
        referenceId: "ref-1",
        file,
      });

      expect(result).toStrictEqual({ ok: false, error: "media_rejected" });
    },
  );

  it("keeps a reason only on a media rejection", async () => {
    replies.push(
      json(413, { error: "quota_exceeded", reason: "unsupported_kind" }),
    );

    const result = await uploadMedia("csrf", tenantId, canvasId, {
      operationId: "op-1",
      referenceId: "ref-1",
      file,
    });

    expect(result).toStrictEqual({ ok: false, error: "quota_exceeded" });
  });

  it.each([
    ["no asset ID", { asset_id: 1 }],
    ["no kind", { kind: undefined }],
    ["no usage", { usage: undefined }],
    ["a malformed usage", { usage: { ...usageBody, used_bytes: "0" } }],
  ])("rejects an answer with %s", async (_, change) => {
    replies.push(json(201, { ...uploaded, ...change }));

    expect(
      await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op-1",
        referenceId: "ref-1",
        file,
      }),
    ).toEqual({ ok: false, error: "unavailable" });
  });

  it("reports a network failure as unavailable", async () => {
    replies.push("network-error");

    expect(
      await uploadMedia("csrf", tenantId, canvasId, {
        operationId: "op-1",
        referenceId: "ref-1",
        file,
      }),
    ).toEqual({ ok: false, error: "unavailable" });
  });
});
