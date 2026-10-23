// Client for the account, session, activation, and canvas API (US-001,
// US-002, US-007). Responses are reduced to a closed set of outcomes; server
// text never reaches the UI.

import type { CanvasObject } from "../canvas/CanvasSurface";

export type { CanvasObject };

export interface Session {
  accountId: string;
  email: string;
  csrfToken: string;
  operator: boolean;
}

export interface OwnCanvas {
  tenantId: string;
  canvasId: string;
  width: number;
  height: number;
  revision: number;
  objects: CanvasObject[];
}

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

export type ApiError =
  | "bad_request"
  | "unauthorized"
  | "forbidden"
  | "not_found"
  | "conflict"
  | "identity_taken"
  | "rate_limited"
  | "unavailable";

export type Result<T> = { ok: true; value: T } | { ok: false; error: ApiError };

const knownErrors = new Set<ApiError>([
  "bad_request",
  "unauthorized",
  "forbidden",
  "not_found",
  "conflict",
  "identity_taken",
  "rate_limited",
]);

async function call(
  method: string,
  path: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<Result<unknown>> {
  let response: Response;
  try {
    response = await fetch(path, {
      method,
      credentials: "same-origin",
      cache: "no-store",
      headers:
        body === undefined
          ? headers
          : { ...headers, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  } catch {
    return { ok: false, error: "unavailable" };
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
  const code =
    typeof payload === "object" && payload !== null && "error" in payload
      ? payload.error
      : undefined;
  return {
    ok: false,
    error: knownErrors.has(code as ApiError)
      ? (code as ApiError)
      : "unavailable",
  };
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

export async function getOwnCanvas(): Promise<Result<OwnCanvas>> {
  const result = await call("GET", "/api/canvas");
  if (!result.ok) return result;
  if (typeof result.value === "object" && result.value !== null) {
    const { tenant_id, canvas_id, width, height, revision, objects } =
      result.value as Record<string, unknown>;
    if (
      typeof tenant_id === "string" &&
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
          ok: true,
          value: {
            tenantId: tenant_id,
            canvasId: canvas_id,
            width: width as number,
            height: height as number,
            revision: revision as number,
            objects: parsed,
          },
        };
      }
    }
  }
  return { ok: false, error: "unavailable" };
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
