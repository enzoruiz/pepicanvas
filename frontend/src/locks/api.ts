// Client for the temporary object locks (US-020). Answers are reduced to a
// closed set of outcomes; server text never reaches the UI, and a holder is
// named only by the account name the server shows.

export interface CanvasRef {
  tenantId: string;
  canvasId: string;
}

export interface LockHolder {
  name: string;
  // The caller's own account holds the lock.
  mine: boolean;
}

export interface LockSessionInfo {
  id: string;
  heartbeatIntervalMs: number;
  ttlMs: number;
}

export interface Renewal {
  renewed: { objectId: string; expiresInMs: number }[];
  lost: { objectId: string; reason: string }[];
}

export interface Acquisition {
  outcome: "granted" | "denied";
  objectId: string;
  holder: LockHolder;
}

export interface LockEntry {
  objectId: string;
  holder: LockHolder;
}

export type ReleaseReason = "finished" | "cancelled";

// network: the request never got an answer, so it may be retried with the
// same operation ID.
export type LockError =
  | "session_gone"
  | "not_holder"
  | "conflict"
  | "forbidden"
  | "unauthorized"
  | "bad_request"
  | "rate_limited"
  | "unavailable"
  | "network";

export type LockResult<T> =
  { ok: true; value: T } | { ok: false; error: LockError };

const knownErrors: Record<string, LockError> = {
  lock_session_gone: "session_gone",
  not_lock_holder: "not_holder",
  conflict: "conflict",
  forbidden: "forbidden",
  unauthorized: "unauthorized",
  bad_request: "bad_request",
  rate_limited: "rate_limited",
  lock_unavailable: "unavailable",
};

function locksPath({ tenantId, canvasId }: CanvasRef): string {
  return `/api/tenants/${encodeURIComponent(tenantId)}/canvases/${encodeURIComponent(canvasId)}/locks`;
}

function sessionPath(canvas: CanvasRef, sessionId: string): string {
  return `${locksPath(canvas)}/sessions/${encodeURIComponent(sessionId)}`;
}

async function send(
  method: string,
  path: string,
  headers: Record<string, string>,
  body?: unknown,
): Promise<LockResult<unknown>> {
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
    return { ok: false, error: "network" };
  }
  if (response.status === 204) return { ok: true, value: undefined };
  let payload: unknown;
  try {
    payload = await response.json();
  } catch {
    payload = undefined;
  }
  if (response.ok) return { ok: true, value: payload };
  const code = isRecord(payload) ? payload.error : undefined;
  return {
    ok: false,
    error: (typeof code === "string" && knownErrors[code]) || "unavailable",
  };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null;
}

function toHolder(value: unknown): LockHolder | undefined {
  if (!isRecord(value)) return undefined;
  const { name, mine } = value;
  return typeof name === "string" && typeof mine === "boolean"
    ? { name, mine }
    : undefined;
}

// parse turns a success body into a value, or an unavailable answer when
// the body does not have the expected shape.
function parse<T>(
  result: LockResult<unknown>,
  read: (value: unknown) => T | undefined,
): LockResult<T> {
  if (!result.ok) return result;
  const value = read(result.value);
  return value === undefined
    ? { ok: false, error: "unavailable" }
    : { ok: true, value };
}

function list<T>(
  value: unknown,
  read: (item: unknown) => T | undefined,
): T[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const items = value.map(read);
  return items.every((item) => item !== undefined) ? items : undefined;
}

export async function openLockSession(
  csrfToken: string,
  canvas: CanvasRef,
): Promise<LockResult<LockSessionInfo>> {
  const result = await send("POST", `${locksPath(canvas)}/sessions`, {
    "X-CSRF-Token": csrfToken,
  });
  return parse(result, (value) => {
    if (!isRecord(value)) return undefined;
    const { lock_session_id, heartbeat_interval_ms, ttl_ms } = value;
    return typeof lock_session_id === "string" &&
      Number.isInteger(heartbeat_interval_ms) &&
      (heartbeat_interval_ms as number) > 0 &&
      Number.isInteger(ttl_ms)
      ? {
          id: lock_session_id,
          heartbeatIntervalMs: heartbeat_interval_ms as number,
          ttlMs: ttl_ms as number,
        }
      : undefined;
  });
}

export async function heartbeatLocks(
  csrfToken: string,
  canvas: CanvasRef,
  sessionId: string,
): Promise<LockResult<Renewal>> {
  const result = await send(
    "POST",
    `${sessionPath(canvas, sessionId)}/heartbeat`,
    { "X-CSRF-Token": csrfToken },
  );
  return parse(result, (value) => {
    if (!isRecord(value)) return undefined;
    const renewed = list(value.renewed, (item) =>
      isRecord(item) &&
      typeof item.object_id === "string" &&
      Number.isInteger(item.expires_in_ms)
        ? {
            objectId: item.object_id,
            expiresInMs: item.expires_in_ms as number,
          }
        : undefined,
    );
    const lost = list(value.lost, (item) =>
      isRecord(item) &&
      typeof item.object_id === "string" &&
      typeof item.reason === "string"
        ? { objectId: item.object_id, reason: item.reason }
        : undefined,
    );
    return renewed && lost ? { renewed, lost } : undefined;
  });
}

export async function acquireLock(
  csrfToken: string,
  canvas: CanvasRef,
  sessionId: string,
  operationId: string,
  objectId: string,
): Promise<LockResult<Acquisition>> {
  const result = await send(
    "POST",
    `${sessionPath(canvas, sessionId)}/acquire`,
    { "X-CSRF-Token": csrfToken },
    { operation_id: operationId, object_id: objectId },
  );
  return parse(result, (value) => {
    if (!isRecord(value)) return undefined;
    const { outcome, object_id } = value;
    const holder = toHolder(value.holder);
    return (outcome === "granted" || outcome === "denied") &&
      typeof object_id === "string" &&
      holder
      ? { outcome, objectId: object_id, holder }
      : undefined;
  });
}

export async function releaseLock(
  csrfToken: string,
  canvas: CanvasRef,
  sessionId: string,
  operationId: string,
  objectId: string,
  reason: ReleaseReason,
): Promise<LockResult<undefined>> {
  const result = await send(
    "POST",
    `${sessionPath(canvas, sessionId)}/release`,
    { "X-CSRF-Token": csrfToken },
    { operation_id: operationId, object_id: objectId, reason },
  );
  if (!result.ok) return result;
  return isRecord(result.value) && result.value.outcome === "released"
    ? { ok: true, value: undefined }
    : { ok: false, error: "unavailable" };
}

export async function endLockSession(
  csrfToken: string,
  canvas: CanvasRef,
  sessionId: string,
): Promise<LockResult<undefined>> {
  const result = await send("DELETE", sessionPath(canvas, sessionId), {
    "X-CSRF-Token": csrfToken,
  });
  return result.ok ? { ok: true, value: undefined } : result;
}

export async function listLocks(
  canvas: CanvasRef,
): Promise<LockResult<LockEntry[]>> {
  const result = await send("GET", locksPath(canvas), {});
  return parse(result, (value) =>
    isRecord(value)
      ? list(value.locks, (item) => {
          if (!isRecord(item) || typeof item.object_id !== "string") {
            return undefined;
          }
          const holder = toHolder(item.holder);
          return holder ? { objectId: item.object_id, holder } : undefined;
        })
      : undefined,
  );
}
