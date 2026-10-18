// Client for the account and session API (US-001). Responses are reduced to
// a closed set of outcomes; server text never reaches the UI.

export interface Session {
  accountId: string;
  email: string;
  csrfToken: string;
}

export type ApiError =
  | "bad_request"
  | "unauthorized"
  | "forbidden"
  | "conflict"
  | "identity_taken"
  | "rate_limited"
  | "unavailable";

export type Result<T> = { ok: true; value: T } | { ok: false; error: ApiError };

const knownErrors = new Set<ApiError>([
  "bad_request",
  "unauthorized",
  "forbidden",
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
    const { account_id, email, csrf_token } = value as Record<string, unknown>;
    if (
      typeof account_id === "string" &&
      typeof email === "string" &&
      typeof csrf_token === "string"
    ) {
      return {
        ok: true,
        value: { accountId: account_id, email, csrfToken: csrf_token },
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
