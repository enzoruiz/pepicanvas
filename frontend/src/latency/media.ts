// The media side of the probe page: a media command has rendered when the
// element reports the new state, not when play() or pause() returns.

export type Media = Pick<
  HTMLMediaElement,
  | "paused"
  | "readyState"
  | "play"
  | "pause"
  | "addEventListener"
  | "removeEventListener"
>;

// HTMLMediaElement.HAVE_ENOUGH_DATA, the state canplaythrough announces.
const haveEnoughData = 4;

export function applyMedia(
  media: Media,
  action: "play" | "pause",
): Promise<void> {
  // An element already in the requested state fires no event, so waiting
  // for one would never end.
  if (media.paused === (action === "pause")) return Promise.resolve();
  const reached = once(media, action === "play" ? "playing" : "pause");
  if (action === "play") {
    return Promise.all([media.play(), reached]).then(() => undefined);
  }
  media.pause();
  return reached;
}

export function whenLoaded(media: Media): Promise<void> {
  if (media.readyState >= haveEnoughData) return Promise.resolve();
  return new Promise((resolve, reject) => {
    const loaded = () => {
      media.removeEventListener("error", failed);
      resolve();
    };
    const failed = () => {
      media.removeEventListener("canplaythrough", loaded);
      reject(new Error("media source failed to load"));
    };
    media.addEventListener("canplaythrough", loaded, { once: true });
    media.addEventListener("error", failed, { once: true });
  });
}

function once(media: Media, type: string): Promise<void> {
  return new Promise((resolve) => {
    media.addEventListener(type, () => resolve(), { once: true });
  });
}
