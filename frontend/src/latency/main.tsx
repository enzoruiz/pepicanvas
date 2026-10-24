// The TE-010 probe page: it subscribes to the harness event stream, renders
// box events on the shared CanvasSurface and media events on one audio
// element, and exposes window.__pepicanvasLatency to the latency runner.

import { flushSync } from "react-dom";
import { createRoot } from "react-dom/client";

import { CanvasSurface } from "../canvas/CanvasSurface";
import {
  emptyBoxes,
  mediaAction,
  parseCanvasEvent,
  reduceBoxes,
  type CanvasEvent,
} from "./canvasEvents";
import { trackDisconnects } from "./disconnects";
import { markRendered } from "./markRendered";
import { applyMedia, whenLoaded } from "./media";
import { measurePings, type Ping } from "./pings";
import { createRenderRegistry, type RenderMark } from "./renderRegistry";
import { generateWav } from "./wav";

interface LatencyProbe {
  ready(): Promise<void>;
  pings(n: number): Promise<Ping[]>;
  rendered(operationId: string): Promise<RenderMark>;
  disconnects(): number;
}

declare global {
  interface Window {
    __pepicanvasLatency?: LatencyProbe;
  }
}

const canvasRoot = document.querySelector("#latency-root");

if (!(canvasRoot instanceof HTMLElement)) {
  throw new Error("Latency root element not found");
}

const root = createRoot(canvasRoot);
let boxes = emptyBoxes;
const renderBoxes = () =>
  flushSync(() =>
    root.render(
      <CanvasSurface width={1280} height={720} objects={boxes.objects} />,
    ),
  );
renderBoxes();

// Muted media may play without a user gesture in every client of the
// matrix; play and pause events fire the same way.
const audio = document.createElement("audio");
audio.muted = true;
audio.loop = true;
audio.preload = "auto";
document.body.append(audio);
const audioLoaded = whenLoaded(audio);
audio.src = URL.createObjectURL(
  new Blob([generateWav({ sampleRate: 8000, seconds: 2, frequency: 220 })], {
    type: "audio/wav",
  }),
);
audio.load();

const now = () => performance.now();
const registry = createRenderRegistry();

async function render(event: CanvasEvent): Promise<void> {
  const action = mediaAction(event);
  if (action === undefined) {
    boxes = reduceBoxes(boxes, event);
    renderBoxes();
  } else {
    await applyMedia(audio, action);
  }
  registry.record(event.operationId, {
    renderedAtMs: await markRendered(requestAnimationFrame, now),
    correlationId: event.correlationId,
    sequence: event.sequence,
  });
}

const events = new EventSource("/probe/events");
const disconnects = trackDisconnects(events);
const streamReady = new Promise<void>((resolve) => {
  events.addEventListener("ready", () => resolve(), { once: true });
});
// Events render one at a time in sequence order, as the server sends them.
let rendering = Promise.resolve();
events.addEventListener("canvas", (message) => {
  let event: CanvasEvent;
  try {
    event = parseCanvasEvent((message as MessageEvent<string>).data);
  } catch (error) {
    console.error(error);
    return;
  }
  rendering = rendering.then(() =>
    render(event).catch((error: unknown) =>
      registry.fail(
        event.operationId,
        error instanceof Error ? error : new Error(String(error)),
      ),
    ),
  );
});

const ready = Promise.all([streamReady, audioLoaded]).then(() => undefined);

window.__pepicanvasLatency = {
  ready: () => ready,
  pings: (n) => measurePings((input, init) => fetch(input, init), now, n),
  rendered: (operationId) => registry.rendered(operationId),
  disconnects,
};
