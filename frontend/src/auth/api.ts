// Client for the account, session, activation, deactivation, invitation,
// membership, canvas, and media API (US-001, US-002, US-003, US-004,
// US-005, US-007, US-016). Responses are reduced to a closed set of outcomes;
// server text never reaches the UI.

import type { CanvasObject } from "../canvas/CanvasSurface";

export type { CanvasObject };

export interface Session {
  accountId: string;
  email: string;
  csrfToken: string;
  operator: boolean;
}

// A canvas read through the own-canvas route or the tenant route.
export interface Canvas {
  tenantId: string;
  canvasId: string;
  width: number;
  height: number;
  revision: number;
  objects: CanvasObject[];
}

export type OwnCanvas = Canvas;

export interface Dimensions {
  canvasId: string;
  width: number;
  height: number;
  revision: number;
}

export interface Activation {
  accountId: string;
  tenantId: string;
  canvasId: string;
  created: boolean;
}

export interface Deactivation {
  accountId: string;
  tenantId: string;
  canvasId: string;
  deactivated: boolean;
}

export interface Invitation {
  invitationId: string;
  createdAt: string;
  expiresAt: string;
}

export interface Revocation {
  invitationId: string;
  status: "revoked" | "accepted";
}

export interface Acceptance {
  tenantId: string;
  canvasId: string;
}

export interface Membership {
  tenantId: string;
  canvasId: string;
  role: "moderator";
}

// The aggregate storage accounting of a canvas (US-016). It never names an
// asset or the size of one.
export interface MediaUsage {
  quotaBytes: number;
  usedBytes: number;
  reservedBytes: number;
  availableBytes: number;
}

export interface UploadedMedia {
  assetId: string;
  kind: string;
  usage: MediaUsage;
}

export interface MediaUpload {
  operationId: string;
  referenceId: string;
  file: File;
  replace?: boolean;
}

export type ApiError =
  | "bad_request"
  | "unauthorized"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "identity_taken"
  | "rate_limited"
  | "subscription_inactive"
  | "unavailable";

export type Result<T> = { ok: true; value: T } | { ok: false; error: ApiError };

// The media routes add their own closed codes (US-016).
export type MediaError =
  | ApiError
  | "length_required"
  | "quota_exceeded"
  | "media_rejected"
  | "upload_in_progress"
  | "reference_exists"
  | "upload_failed"
  | "media_unavailable";

// The closed rejection categories of media inspection.
export type MediaRejection =
  | "invalid_limits"
  | "unsupported_kind"
  | "invalid_metadata"
  | "input_too_large"
  | "dimensions_exceeded"
  | "duration_exceeded"
  | "frame_count_exceeded"
  | "stream_count_exceeded";

// reason is set only on a media_rejected failure with a known category.
export type MediaResult<T> =
  | { ok: true; value: T }
  | { ok: false; error: MediaError; reason?: MediaRejection };

const knownErrors = new Set<ApiError>([
  "bad_request",
  "unauthorized",
  "forbidden",
  "not_found",
  "conflict",
  "identity_taken",
  "rate_limited",
  "subscription_inactive",
]);

const knownMediaErrors = new Set<MediaError>([
  ...knownErrors,
  "length_required",
  "quota_exceeded",
  "media_rejected",
  "upload_in_progress",
  "reference_exists",
  "upload_failed",
  "media_unavailable",
]);

const knownRejections = new Set<MediaRejection>([
  "invalid_limits",
  "unsupported_kind",
  "invalid_metadata",
  "input_too_large",
  "dimensions_exceeded",
  "duration_exceeded",
  "frame_count_exceeded",
  "stream_count_exceeded",
]);

// A raw answer: the body of a success, or the untrusted error code and
// reason of a failure. A network failure has neither.
type Answer =
  | { ok: true; value: unknown }
  | { ok: false; code?: unknown; reason?: unknown };

async function send(
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: BodyInit,
): Promise<Answer> {
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      credentials: "same-origin",
      cache: "no-store",
      headers,
      body,
    });
  } catch {
    return { ok: false };
  }
  if (response.status === 204) {
    return { ok: true, value: undefined };
  }
  let payload: unknown;
  try {
    payload = await response.json();
  } catch {
    payload = undefined;
  }
  if (response.ok) {
    return { ok: true, value: payload };
  }
  const { error, reason } =
    typeof payload === "object" && payload !== null
      ? (payload as Record<string, unknown>)
      : {};
  return { ok: false, code: error, reason };
}

async function call(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<Result<unknown>> {
  const answer =
    body === undefined
      ? await send(method, path, headers)
      : await send(
          method,
          path,
          { ...headers, "Content-Type": "application/json" },
          JSON.stringify(body),
        );
  if (answer.ok) return answer;
  return {
    ok: false,
    error: knownErrors.has(answer.code as ApiError)
      ? (answer.code as ApiError)
      : "unavailable",
  };
}

async function mediaCall(
  method: string,
  path: string,
  headers: Record<string, string> = {},
  body?: BodyInit,
): Promise<MediaResult<unknown>> {
  const answer = await send(method, path, headers, body);
  if (answer.ok) return answer;
  const error = knownMediaErrors.has(answer.code as MediaError)
    ? (answer.code as MediaError)
    : "unavailable";
  return error === "media_rejected" &&
    knownRejections.has(answer.reason as MediaRejection)
    ? { ok: false, error, reason: answer.reason as MediaRejection }
    : { ok: false, error };
}

function toSession(value: unknown): Result<Session> {
  if (typeof value === "object" && value !== null) {
    const { account_id, email, csrf_token, operator } = value as Record<
      string,
      unknown
    >;
    if (
      typeof account_id === "string" &&
      typeof email === "string" &&
      typeof csrf_token === "string" &&
      typeof operator === "boolean"
    ) {
      return {
        ok: true,
        value: {
          accountId: account_id,
          email,
          csrfToken: csrf_token,
          operator,
        },
      };
    }
  }
  return { ok: false, error: "unavailable" };
}

export async function getSession(): Promise<Result<Session>> {
  const result = await call("GET", "/api/session");
  return result.ok ? toSession(result.value) : result;
}

export async function signIn(
  email: string,
  password: string,
): Promise<Result<Session>> {
  const result = await call("POST", "/api/sessions", { email, password });
  return result.ok ? toSession(result.value) : result;
}

export async function register(
  operationId: string,
  email: string,
  password: string,
): Promise<Result<undefined>> {
  const result = await call("POST", "/api/accounts", {
    operation_id: operationId,
    email,
    password,
  });
  return result.ok ? { ok: true, value: undefined } : result;
}

export async function signOut(csrfToken: string): Promise<Result<undefined>> {
  const result = await call("DELETE", "/api/session", undefined, {
    "X-CSRF-Token": csrfToken,
  });
  return result.ok ? { ok: true, value: undefined } : result;
}

function toCanvasObject(value: unknown): CanvasObject | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const { id, kind, content, x, y, width, height, rotation, z } =
    value as Record<string, unknown>;
  if (
    typeof id === "string" &&
    typeof kind === "string" &&
    typeof content === "string" &&
    Number.isInteger(x) &&
    Number.isInteger(y) &&
    Number.isInteger(width) &&
    Number.isInteger(height) &&
    typeof rotation === "number" &&
    Number.isFinite(rotation) &&
    Number.isInteger(z)
  ) {
    return {
      id,
      kind,
      content,
      x: x as number,
      y: y as number,
      width: width as number,
      height: height as number,
      rotation,
      z: z as number,
    };
  }
  return undefined;
}

// Reads the canvas fields shared by both canvas routes; the tenant route
// answers no tenant_id, so the caller supplies the tenant it asked for.
function toCanvas(value: unknown, tenantId: string): Canvas | undefined {
  if (typeof value !== "object" || value === null) return undefined;
  const { canvas_id, width, height, revision, objects } = value as Record<
    string,
    unknown
  >;
  if (
    typeof canvas_id === "string" &&
    Number.isInteger(width) &&
    Number.isInteger(height) &&
    Number.isInteger(revision) &&
    Array.isArray(objects)
  ) {
    const parsed = objects.map(toCanvasObject);
    if (
      parsed.every((object): object is CanvasObject => object !== undefined)
    ) {
      return {
        tenantId,
        canvasId: canvas_id,
        width: width as number,
        height: height as number,
        revision: revision as number,
        objects: parsed,
      };
    }
  }
  return undefined;
}

export async function getOwnCanvas(): Promise<Result<OwnCanvas>> {
  const result = await call("GET", "/api/canvas");
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { tenant_id } = result.value as Record<string, unknown>;
    const canvas =
      typeof tenant_id === "string"
        ? toCanvas(result.value, tenant_id)
        : undefined;
    if (canvas) return { ok: true, value: canvas };
  }
  return { ok: false, error: "unavailable" };
}

// Reads a canvas the account may view through its membership (US-005).
export async function getTenantCanvas(
  tenantId: string,
  canvasId: string,
): Promise<Result<Canvas>> {
  const result = await call(
    "GET",
    `/api/tenants/${encodeURIComponent(tenantId)}/canvases/${encodeURIComponent(canvasId)}`,
  );
  if (!result.ok) return result;
  const canvas = toCanvas(result.value, tenantId);
  return canvas?.canvasId === canvasId
    ? { ok: true, value: canvas }
    : { ok: false, error: "unavailable" };
}

export async function setDimensions(
  csrfToken: string,
  tenantId: string,
  canvasId: string,
  operationId: string,
  width: number,
  height: number,
): Promise<Result<Dimensions>> {
  const result = await call(
    "PUT",
    `/api/tenants/${encodeURIComponent(tenantId)}/canvases/${encodeURIComponent(canvasId)}/dimensions`,
    { operation_id: operationId, width, height },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const {
      canvas_id,
      width: confirmedWidth,
      height: confirmedHeight,
      revision,
    } = result.value as Record<string, unknown>;
    if (
      typeof canvas_id === "string" &&
      Number.isInteger(confirmedWidth) &&
      Number.isInteger(confirmedHeight) &&
      Number.isInteger(revision)
    ) {
      return {
        ok: true,
        value: {
          canvasId: canvas_id,
          width: confirmedWidth as number,
          height: confirmedHeight as number,
          revision: revision as number,
        },
      };
    }
  }
  return { ok: false, error: "unavailable" };
}

export async function activate(
  csrfToken: string,
  operationId: string,
  email: string,
): Promise<Result<Activation>> {
  const result = await call(
    "POST",
    "/api/activations",
    { operation_id: operationId, email },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { account_id, tenant_id, canvas_id, created } =
      result.value as Record<string, unknown>;
    if (
      typeof account_id === "string" &&
      typeof tenant_id === "string" &&
      typeof canvas_id === "string" &&
      typeof created === "boolean"
    ) {
      return {
        ok: true,
        value: {
          accountId: account_id,
          tenantId: tenant_id,
          canvasId: canvas_id,
          created,
        },
      };
    }
  }
  return { ok: false, error: "unavailable" };
}

export async function deactivate(
  csrfToken: string,
  operationId: string,
  email: string,
): Promise<Result<Deactivation>> {
  const result = await call(
    "POST",
    "/api/deactivations",
    { operation_id: operationId, email },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { account_id, tenant_id, canvas_id, deactivated } =
      result.value as Record<string, unknown>;
    if (
      typeof account_id === "string" &&
      typeof tenant_id === "string" &&
      typeof canvas_id === "string" &&
      typeof deactivated === "boolean"
    ) {
      return {
        ok: true,
        value: {
          accountId: account_id,
          tenantId: tenant_id,
          canvasId: canvas_id,
          deactivated,
        },
      };
    }
  }
  return { ok: false, error: "unavailable" };
}

// An invitation token is pci_ followed by 256 random bits in unpadded
// base64url (43 characters). It is generated here so a retried creation
// carries the same token; the server stores only its hash.
export function generateInvitationToken(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const base64 = btoa(String.fromCharCode(...bytes));
  return `pci_${base64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "")}`;
}

const invitationIdPattern = /^[0-9a-f]{32}$/;

function isTimestamp(value: unknown): value is string {
  return typeof value === "string" && !Number.isNaN(Date.parse(value));
}

function toInvitation(value: unknown): Invitation | undefined {
  if (typeof value === "object" && value !== null) {
    const { invitation_id, created_at, expires_at } = value as Record<
      string,
      unknown
    >;
    if (
      typeof invitation_id === "string" &&
      invitationIdPattern.test(invitation_id) &&
      isTimestamp(created_at) &&
      isTimestamp(expires_at)
    ) {
      return {
        invitationId: invitation_id,
        createdAt: created_at,
        expiresAt: expires_at,
      };
    }
  }
  return undefined;
}

// The token travels only in the JSON body, never in the URL.
export async function createInvitation(
  csrfToken: string,
  operationId: string,
  token: string,
): Promise<Result<Invitation>> {
  const result = await call(
    "POST",
    "/api/invitations",
    { operation_id: operationId, token },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  const invitation = toInvitation(result.value);
  return invitation
    ? { ok: true, value: invitation }
    : { ok: false, error: "unavailable" };
}

export async function listInvitations(): Promise<Result<Invitation[]>> {
  const result = await call("GET", "/api/invitations");
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { invitations } = result.value as Record<string, unknown>;
    if (Array.isArray(invitations)) {
      const parsed = invitations.map(toInvitation);
      if (parsed.every((invitation) => invitation !== undefined)) {
        return { ok: true, value: parsed };
      }
    }
  }
  return { ok: false, error: "unavailable" };
}

export async function revokeInvitation(
  csrfToken: string,
  operationId: string,
  invitationId: string,
): Promise<Result<Revocation>> {
  const result = await call(
    "POST",
    `/api/invitations/${encodeURIComponent(invitationId)}/revoke`,
    { operation_id: operationId },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { invitation_id, status } = result.value as Record<string, unknown>;
    if (
      typeof invitation_id === "string" &&
      invitationIdPattern.test(invitation_id) &&
      (status === "revoked" || status === "accepted")
    ) {
      return { ok: true, value: { invitationId: invitation_id, status } };
    }
  }
  return { ok: false, error: "unavailable" };
}

// Tenant and canvas IDs are 128-bit hex values; anything else is rejected
// before it can reach a URL path.
const resourceIdPattern = /^[0-9a-f]{32}$/;

function isResourceId(value: unknown): value is string {
  return typeof value === "string" && resourceIdPattern.test(value);
}

// The token travels only in the JSON body, never in the URL.
export async function acceptInvitation(
  csrfToken: string,
  operationId: string,
  token: string,
): Promise<Result<Acceptance>> {
  const result = await call(
    "POST",
    "/api/invitations/accept",
    { operation_id: operationId, token },
    { "X-CSRF-Token": csrfToken },
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { tenant_id, canvas_id } = result.value as Record<string, unknown>;
    if (isResourceId(tenant_id) && isResourceId(canvas_id)) {
      return { ok: true, value: { tenantId: tenant_id, canvasId: canvas_id } };
    }
  }
  return { ok: false, error: "unavailable" };
}

function toMembership(value: unknown): Membership | undefined {
  if (typeof value === "object" && value !== null) {
    const { tenant_id, canvas_id, role } = value as Record<string, unknown>;
    if (
      isResourceId(tenant_id) &&
      isResourceId(canvas_id) &&
      role === "moderator"
    ) {
      return { tenantId: tenant_id, canvasId: canvas_id, role };
    }
  }
  return undefined;
}

function isByteCount(value: unknown): value is number {
  return Number.isSafeInteger(value) && (value as number) >= 0;
}

function toMediaUsage(value: unknown): MediaUsage | undefined {
  if (typeof value === "object" && value !== null) {
    const { quota_bytes, used_bytes, reserved_bytes, available_bytes } =
      value as Record<string, unknown>;
    if (
      isByteCount(quota_bytes) &&
      isByteCount(used_bytes) &&
      isByteCount(reserved_bytes) &&
      isByteCount(available_bytes)
    ) {
      return {
        quotaBytes: quota_bytes,
        usedBytes: used_bytes,
        reservedBytes: reserved_bytes,
        availableBytes: available_bytes,
      };
    }
  }
  return undefined;
}

export async function listMemberships(): Promise<Result<Membership[]>> {
  const result = await call("GET", "/api/memberships");
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { memberships } = result.value as Record<string, unknown>;
    if (Array.isArray(memberships)) {
      const parsed = memberships.map(toMembership);
      if (parsed.every((membership) => membership !== undefined)) {
        return { ok: true, value: parsed };
      }
    }
  }
  return { ok: false, error: "unavailable" };
}

function mediaPath(tenantId: string, canvasId: string, route: string): string {
  return `/api/tenants/${encodeURIComponent(tenantId)}/canvases/${encodeURIComponent(canvasId)}/media/${route}`;
}

export async function getMediaUsage(
  tenantId: string,
  canvasId: string,
): Promise<MediaResult<MediaUsage>> {
  const result = await mediaCall("GET", mediaPath(tenantId, canvasId, "usage"));
  if (!result.ok) return result;
  const usage = toMediaUsage(result.value);
  return usage
    ? { ok: true, value: usage }
    : { ok: false, error: "unavailable" };
}

// The file travels as the raw request body; the browser sets its exact
// Content-Length, which the server reserves as the declared size.
export async function uploadMedia(
  csrfToken: string,
  tenantId: string,
  canvasId: string,
  upload: MediaUpload,
): Promise<MediaResult<UploadedMedia>> {
  const query = new URLSearchParams({
    operation_id: upload.operationId,
    reference_id: upload.referenceId,
  });
  if (upload.replace) query.set("replace", "true");
  const result = await mediaCall(
    "POST",
    `${mediaPath(tenantId, canvasId, "uploads")}?${query.toString()}`,
    {
      "X-CSRF-Token": csrfToken,
      "Content-Type": "application/octet-stream",
    },
    upload.file,
  );
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { asset_id, kind, usage } = result.value as Record<string, unknown>;
    const parsed = toMediaUsage(usage);
    if (typeof asset_id === "string" && typeof kind === "string" && parsed) {
      return { ok: true, value: { assetId: asset_id, kind, usage: parsed } };
    }
  }
  return { ok: false, error: "unavailable" };
}
