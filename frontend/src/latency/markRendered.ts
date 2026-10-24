// The render instant of TE-010: the second animation frame after a DOM
// commit or a media event. The first callback runs before the browser paints
// the change; the second runs after that paint, so its time bounds it.

export type RequestFrame = (callback: FrameRequestCallback) => unknown;

export function markRendered(
  raf: RequestFrame,
  now: () => number,
): Promise<number> {
  return new Promise((resolve) => {
    raf(() => {
      raf(() => resolve(now()));
    });
  });
}
