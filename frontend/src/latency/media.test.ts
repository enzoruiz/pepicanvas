import { describe, expect, it, vi } from "vitest";

import { applyMedia, whenLoaded, type Media } from "./media";

class FakeMedia extends EventTarget {
  paused = true;
  readyState = 0;
  play = vi.fn(() => Promise.resolve());
  pause = vi.fn(() => undefined);
}

function settled(promise: Promise<unknown>): () => boolean {
  let done = false;
  void promise.then(
    () => (done = true),
    () => (done = true),
  );
  return () => done;
}

const asMedia = (fake: FakeMedia) => fake as unknown as Media;

describe("applyMedia", () => {
  it("plays and waits for the playing event", async () => {
    const media = new FakeMedia();
    const applied = applyMedia(asMedia(media), "play");
    const done = settled(applied);

    await Promise.resolve();
    expect(media.play).toHaveBeenCalledTimes(1);
    expect(done()).toBe(false);
    media.dispatchEvent(new Event("playing"));
    await expect(applied).resolves.toBeUndefined();
  });

  it("pauses and waits for the pause event", async () => {
    const media = new FakeMedia();
    media.paused = false;
    const applied = applyMedia(asMedia(media), "pause");
    const done = settled(applied);

    await Promise.resolve();
    expect(media.pause).toHaveBeenCalledTimes(1);
    expect(done()).toBe(false);
    media.dispatchEvent(new Event("pause"));
    await expect(applied).resolves.toBeUndefined();
  });

  it("rejects when the browser refuses to play", async () => {
    const media = new FakeMedia();
    media.play.mockReturnValue(Promise.reject(new Error("NotAllowedError")));

    await expect(applyMedia(asMedia(media), "play")).rejects.toThrow(
      "NotAllowedError",
    );
  });

  it("resolves at once when the element is already in the requested state", async () => {
    const playing = new FakeMedia();
    playing.paused = false;
    const paused = new FakeMedia();

    await expect(applyMedia(asMedia(playing), "play")).resolves.toBeUndefined();
    await expect(applyMedia(asMedia(paused), "pause")).resolves.toBeUndefined();
    expect(playing.play).not.toHaveBeenCalled();
    expect(paused.pause).not.toHaveBeenCalled();
  });
});

describe("whenLoaded", () => {
  it("waits for canplaythrough", async () => {
    const media = new FakeMedia();
    const loaded = whenLoaded(asMedia(media));
    const done = settled(loaded);

    await Promise.resolve();
    expect(done()).toBe(false);
    media.dispatchEvent(new Event("canplay"));
    await Promise.resolve();
    expect(done()).toBe(false);
    media.dispatchEvent(new Event("canplaythrough"));
    await expect(loaded).resolves.toBeUndefined();
  });

  it("resolves at once when the element already has enough data", async () => {
    const media = new FakeMedia();
    media.readyState = 4;

    await expect(whenLoaded(asMedia(media))).resolves.toBeUndefined();
  });

  it("rejects when the source fails to load", async () => {
    const media = new FakeMedia();
    const loaded = whenLoaded(asMedia(media));
    media.dispatchEvent(new Event("error"));

    await expect(loaded).rejects.toThrow(/media/);
  });
});
