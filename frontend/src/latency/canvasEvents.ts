// The probe page's view of the harness SSE `canvas` events: parsing a frame
// and folding box payloads onto CanvasSurface objects. Payloads are opaque to
// the server, so a payload this page does not know leaves the boxes as they
// are.

import type { CanvasObject } from "../canvas/CanvasSurface";

export interface CanvasEvent {
  sequence: number;
  operationId: string;
  objectId: string;
  kind: "set" | "delete";
  payload: Record<string, unknown> | null;
  correlationId: string;
}

export interface BoxState {
  objects: readonly CanvasObject[];
  // created counts every box ever created; it gives each new box its z.
  created: number;
}

export const emptyBoxes: BoxState = { objects: [], created: 0 };

export function parseCanvasEvent(data: string): CanvasEvent {
  let frame: unknown;
  try {
    frame = JSON.parse(data);
  } catch {
    throw new Error("canvas event is not JSON");
  }
  if (typeof frame !== "object" || frame === null || Array.isArray(frame)) {
    throw new Error("canvas event is not an object");
  }
  const { sequence, operationId, objectId, kind, payload, correlationId } =
    frame as Record<string, unknown>;
  if (
    typeof sequence !== "number" ||
    !Number.isSafeInteger(sequence) ||
    typeof operationId !== "string" ||
    typeof objectId !== "string" ||
    typeof correlationId !== "string"
  ) {
    throw new Error("canvas event has invalid fields");
  }
  if (kind === "set" && isRecord(payload)) {
    return { sequence, operationId, objectId, kind, payload, correlationId };
  }
  if (kind === "delete" && payload === null) {
    return { sequence, operationId, objectId, kind, payload, correlationId };
  }
  throw new Error("canvas event has an invalid kind or payload");
}

export function reduceBoxes(state: BoxState, event: CanvasEvent): BoxState {
  if (event.kind === "delete") {
    if (!state.objects.some((object) => object.id === event.objectId)) {
      return state;
    }
    return {
      ...state,
      objects: state.objects.filter((object) => object.id !== event.objectId),
    };
  }
  const payload = event.payload ?? {};
  if (payload.op === "create") {
    const geometry = numbers(payload, [
      "x",
      "y",
      "width",
      "height",
      "rotation",
    ]);
    if (geometry === undefined) return state;
    const created = state.created + 1;
    const box: CanvasObject = {
      id: event.objectId,
      kind: "text",
      content: "",
      x: geometry.x,
      y: geometry.y,
      width: geometry.width,
      height: geometry.height,
      rotation: geometry.rotation,
      z: created,
    };
    return {
      objects: [
        ...state.objects.filter((object) => object.id !== event.objectId),
        box,
      ],
      created,
    };
  }
  const fields = updates[payload.op as string];
  if (fields === undefined) return state;
  const change = numbers(payload, fields);
  if (change === undefined) return state;
  const index = state.objects.findIndex(
    (object) => object.id === event.objectId,
  );
  if (index < 0) return state;
  const objects = [...state.objects];
  objects[index] = { ...objects[index], ...change };
  return { ...state, objects };
}

// applyBoxEvent folds a box event and reports whether it changed the boxes.
// reduceBoxes returns the same state on every no-op path, so an event that
// changed nothing has no render to mark.
export function applyBoxEvent(
  state: BoxState,
  event: CanvasEvent,
): { boxes: BoxState; changed: boolean } {
  const boxes = reduceBoxes(state, event);
  return { boxes, changed: boxes !== state };
}

export function mediaAction(event: CanvasEvent): "play" | "pause" | undefined {
  const payload = event.payload;
  if (payload?.op !== "media") return undefined;
  return payload.action === "play" || payload.action === "pause"
    ? payload.action
    : undefined;
}

type Geometry = "x" | "y" | "width" | "height" | "rotation";

const updates: Record<string, readonly Geometry[] | undefined> = {
  move: ["x", "y"],
  resize: ["width", "height"],
  rotate: ["rotation"],
};

function numbers<K extends Geometry>(
  payload: Record<string, unknown>,
  fields: readonly K[],
): Record<K, number> | undefined {
  const values = {} as Record<K, number>;
  for (const field of fields) {
    const value = payload[field];
    if (typeof value !== "number" || !Number.isFinite(value)) return undefined;
    values[field] = value;
  }
  return values;
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
