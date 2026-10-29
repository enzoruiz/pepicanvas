// The editor's side of the temporary object locks (US-020). A controller
// opens a lock session when it first needs a lock, heartbeats at the
// interval the server sets while the session is open, and ends the session
// once it holds no lock and has nothing pending. A session the server ended
// takes every lock with it: the controller forgets them and the next
// acquire opens a new session, so a lock is never assumed to survive a
// reconnection.

import {
  type Acquisition,
  acquireLock,
  type CanvasRef,
  endLockSession,
  heartbeatLocks,
  type LockError,
  type LockHolder,
  type LockResult,
  type LockSessionInfo,
  openLockSession,
  type ReleaseReason,
  releaseLock,
  type Renewal,
} from "./api";

// LockClient is the lock API of one canvas.
export interface LockClient {
  open(): Promise<LockResult<LockSessionInfo>>;
  heartbeat(sessionId: string): Promise<LockResult<Renewal>>;
  acquire(
    sessionId: string,
    operationId: string,
    objectId: string,
  ): Promise<LockResult<Acquisition>>;
  release(
    sessionId: string,
    operationId: string,
    objectId: string,
    reason: ReleaseReason,
  ): Promise<LockResult<undefined>>;
  end(sessionId: string): Promise<LockResult<undefined>>;
}

export function httpLockClient(
  csrfToken: string,
  canvas: CanvasRef,
): LockClient {
  return {
    open: () => openLockSession(csrfToken, canvas),
    heartbeat: (sessionId) => heartbeatLocks(csrfToken, canvas, sessionId),
    acquire: (sessionId, operationId, objectId) =>
      acquireLock(csrfToken, canvas, sessionId, operationId, objectId),
    release: (sessionId, operationId, objectId, reason) =>
      releaseLock(csrfToken, canvas, sessionId, operationId, objectId, reason),
    end: (sessionId) => endLockSession(csrfToken, canvas, sessionId),
  };
}

// Why a held lock was lost without being released: it saw no accepted
// heartbeat for its lifetime, its holder lost access to it, or its session
// ended.
export type LossReason = "expired" | "unauthorized" | "disconnected";

export type AcquireOutcome =
  | { kind: "granted" }
  | { kind: "denied"; holder: LockHolder }
  | { kind: "failed"; error: LockError };

export type ReleaseOutcome =
  { kind: "released" } | { kind: "failed"; error: LockError };

export interface LockControllerOptions {
  client: LockClient;
  newOperationId?: () => string;
  onLost?: (objectId: string, reason: LossReason) => void;
  // How many times an unanswered request is sent again under the same
  // operation ID.
  networkRetries?: number;
}

interface OpenSession {
  id: string;
  timer: ReturnType<typeof setInterval>;
}

// Errors after which the server holds no lock of the session.
const endedErrors = new Set<LockError>([
  "session_gone",
  "forbidden",
  "unauthorized",
]);

export class LockController {
  readonly #client: LockClient;
  readonly #newOperationId: () => string;
  readonly #onLost: (objectId: string, reason: LossReason) => void;
  readonly #retries: number;
  #session: OpenSession | null = null;
  #opening: Promise<LockResult<OpenSession>> | null = null;
  #held = new Set<string>();
  #pending = 0;
  #beating = false;
  #disposed = false;

  constructor({
    client,
    newOperationId = () => crypto.randomUUID(),
    onLost = () => undefined,
    networkRetries = 2,
  }: LockControllerOptions) {
    this.#client = client;
    this.#newOperationId = newOperationId;
    this.#onLost = onLost;
    this.#retries = networkRetries;
  }

  // The objects whose locks the controller holds, in acquire order.
  held(): string[] {
    return [...this.#held];
  }

  async acquire(objectId: string): Promise<AcquireOutcome> {
    if (this.#disposed) return { kind: "failed", error: "unavailable" };
    this.#pending++;
    try {
      const opened = await this.#ensureSession();
      if (!opened.ok) return { kind: "failed", error: opened.error };
      const session = opened.value;
      const operationId = this.#newOperationId();
      const result = await this.#retry(() =>
        this.#client.acquire(session.id, operationId, objectId),
      );
      if (this.#session !== session) {
        // The session ended meanwhile, and any lock with it.
        return { kind: "failed", error: "session_gone" };
      }
      if (!result.ok) {
        if (endedErrors.has(result.error)) this.#drop();
        return { kind: "failed", error: result.error };
      }
      if (result.value.outcome === "denied") {
        return { kind: "denied", holder: result.value.holder };
      }
      this.#held.add(objectId);
      return { kind: "granted" };
    } finally {
      this.#pending--;
      this.#closeIfIdle();
    }
  }

  // finish releases a lock after its edit completed.
  finish(objectId: string): Promise<ReleaseOutcome> {
    return this.#release(objectId, "finished");
  }

  // cancel releases a lock after its edit was abandoned.
  cancel(objectId: string): Promise<ReleaseOutcome> {
    return this.#release(objectId, "cancelled");
  }

  // dispose forgets every lock, stops heartbeating, and ends the session
  // without waiting for the answer. Later acquires fail.
  dispose(): void {
    this.#disposed = true;
    this.#held.clear();
    const session = this.#session;
    if (session) {
      this.#forget(session);
      void this.#client.end(session.id);
    }
  }

  async #release(
    objectId: string,
    reason: ReleaseReason,
  ): Promise<ReleaseOutcome> {
    const session = this.#session;
    if (!this.#held.delete(objectId) || !session) {
      return { kind: "failed", error: "not_holder" };
    }
    this.#pending++;
    try {
      const operationId = this.#newOperationId();
      const result = await this.#retry(() =>
        this.#client.release(session.id, operationId, objectId, reason),
      );
      if (result.ok) return { kind: "released" };
      if (this.#session === session && endedErrors.has(result.error)) {
        this.#drop();
      }
      return { kind: "failed", error: result.error };
    } finally {
      this.#pending--;
      this.#closeIfIdle();
    }
  }

  #ensureSession(): Promise<LockResult<OpenSession>> {
    if (this.#session) {
      return Promise.resolve({ ok: true, value: this.#session });
    }
    this.#opening ??= this.#client.open().then((result) => {
      this.#opening = null;
      if (!result.ok) return result;
      if (this.#disposed) {
        void this.#client.end(result.value.id);
        return { ok: false, error: "unavailable" };
      }
      const session: OpenSession = {
        id: result.value.id,
        timer: setInterval(
          () => void this.#heartbeat(),
          result.value.heartbeatIntervalMs,
        ),
      };
      this.#session = session;
      return { ok: true, value: session };
    });
    return this.#opening;
  }

  async #heartbeat(): Promise<void> {
    const session = this.#session;
    if (!session || this.#beating) return;
    this.#beating = true;
    try {
      const result = await this.#client.heartbeat(session.id);
      if (this.#session !== session) return;
      if (!result.ok) {
        // An unanswered heartbeat is sent again at the next tick.
        if (endedErrors.has(result.error)) this.#drop();
        return;
      }
      for (const { objectId, reason } of result.value.lost) {
        if (this.#held.delete(objectId)) {
          this.#onLost(objectId, lossReason(reason));
        }
      }
      this.#closeIfIdle();
    } finally {
      this.#beating = false;
    }
  }

  // drop forgets a session the server already ended and every lock it held.
  #drop(): void {
    const session = this.#session;
    if (session) this.#forget(session);
    const lost = [...this.#held];
    this.#held.clear();
    for (const objectId of lost) this.#onLost(objectId, "disconnected");
  }

  #closeIfIdle(): void {
    const session = this.#session;
    if (!session || this.#held.size > 0 || this.#pending > 0) return;
    this.#forget(session);
    void this.#client.end(session.id);
  }

  #forget(session: OpenSession): void {
    clearInterval(session.timer);
    if (this.#session === session) this.#session = null;
  }

  async #retry<T>(send: () => Promise<LockResult<T>>): Promise<LockResult<T>> {
    let result = await send();
    for (
      let attempt = 0;
      attempt < this.#retries && !result.ok && result.error === "network";
      attempt++
    ) {
      result = await send();
    }
    return result;
  }
}

function lossReason(reason: string): LossReason {
  return reason === "expired" || reason === "unauthorized"
    ? reason
    : "disconnected";
}
