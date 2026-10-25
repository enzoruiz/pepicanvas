import { vi } from "vitest";

// A fetch stub that records every request and answers from a queue, so a
// test controls each server reply in order.

export interface Call {
  method: string;
  path: string;
  headers: Record<string, string>;
  body: Record<string, unknown> | undefined;
  // A raw upload body, sent as is.
  raw?: Blob;
}

export type Reply = { status: number; body?: unknown } | "network-error";

export function json(status: number, body?: unknown): Reply {
  return { status, body };
}

export function stubFetch(): { calls: Call[]; replies: Reply[] } {
  const calls: Call[] = [];
  const replies: Reply[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((path: string, init: RequestInit) => {
      calls.push({
        method: init.method ?? "GET",
        path,
        headers: init.headers as Record<string, string>,
        body:
          typeof init.body === "string"
            ? (JSON.parse(init.body) as Record<string, unknown>)
            : undefined,
        raw: init.body instanceof Blob ? init.body : undefined,
      });
      const reply = replies.shift();
      if (reply === undefined) throw new Error(`unexpected ${path}`);
      if (reply === "network-error") {
        return Promise.reject(new TypeError("Failed to fetch"));
      }
      return Promise.resolve(
        new Response(
          reply.body === undefined ? null : JSON.stringify(reply.body),
          { status: reply.status },
        ),
      );
    }),
  );
  return { calls, replies };
}
