// Sequential pings to the harness clock. Each ping is stamped with the page
// clock just before the request and just after the answer, so the evaluator
// can map server time onto page time from the fastest round trip.

export interface Ping {
  clientSentMs: number;
  serverAtNs: number;
  clientReceivedMs: number;
}

const pingPath = "/probe/ping";

export async function measurePings(
  fetchPing: typeof fetch,
  now: () => number,
  n: number,
): Promise<Ping[]> {
  if (!Number.isInteger(n) || n < 1) {
    throw new Error(`ping count must be a positive integer, got ${n}`);
  }
  const pings: Ping[] = [];
  for (let i = 0; i < n; i++) {
    const clientSentMs = now();
    const response = await fetchPing(pingPath, { cache: "no-store" });
    const clientReceivedMs = now();
    if (response.status !== 200) {
      throw new Error(`ping answered ${response.status}`);
    }
    pings.push({
      clientSentMs,
      serverAtNs: parseServerAt(await response.text()),
      clientReceivedMs,
    });
  }
  return pings;
}

function parseServerAt(text: string): number {
  let body: unknown;
  try {
    body = JSON.parse(text);
  } catch {
    throw new Error("ping body is not JSON");
  }
  const serverAtNs =
    typeof body === "object" && body !== null && !Array.isArray(body)
      ? (body as Record<string, unknown>).serverAtNs
      : undefined;
  if (
    typeof serverAtNs !== "number" ||
    !Number.isSafeInteger(serverAtNs) ||
    serverAtNs < 0
  ) {
    throw new Error("ping body has no valid serverAtNs");
  }
  return serverAtNs;
}
