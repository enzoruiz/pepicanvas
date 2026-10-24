// EventSource fires `error` on every lost connection, including the ones it
// retries, so each one is a disconnect for the sample it overlaps.

export function trackDisconnects(
  source: Pick<EventTarget, "addEventListener">,
): () => number {
  let count = 0;
  source.addEventListener("error", () => {
    count += 1;
  });
  return () => count;
}
