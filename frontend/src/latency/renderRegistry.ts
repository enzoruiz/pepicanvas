// Render marks by operation ID. A harness may ask for an operation before or
// after the page rendered it; both get the same mark.

export interface RenderMark {
  renderedAtMs: number;
  correlationId: string;
  sequence: number;
}

export interface RenderRegistry {
  record(operationId: string, mark: RenderMark): void;
  fail(operationId: string, error: Error): void;
  rendered(operationId: string): Promise<RenderMark>;
}

interface Entry {
  promise: Promise<RenderMark>;
  resolve(mark: RenderMark): void;
  reject(error: Error): void;
}

export function createRenderRegistry(): RenderRegistry {
  const entries = new Map<string, Entry>();
  const entry = (operationId: string): Entry => {
    let found = entries.get(operationId);
    if (found === undefined) {
      let resolve!: (mark: RenderMark) => void;
      let reject!: (error: Error) => void;
      const promise = new Promise<RenderMark>((onResolve, onReject) => {
        resolve = onResolve;
        reject = onReject;
      });
      // A failure nobody waits for yet must not surface as unhandled.
      promise.catch(() => undefined);
      found = { promise, resolve, reject };
      entries.set(operationId, found);
    }
    return found;
  };
  return {
    record: (operationId, mark) => entry(operationId).resolve(mark),
    fail: (operationId, error) => entry(operationId).reject(error),
    rendered: (operationId) => entry(operationId).promise,
  };
}
