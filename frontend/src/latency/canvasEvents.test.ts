import { describe, expect, it } from "vitest";

import {
  applyBoxEvent,
  emptyBoxes,
  mediaAction,
  parseCanvasEvent,
  reduceBoxes,
  type CanvasEvent,
} from "./canvasEvents";

const correlationId = "0123456789abcdef0123456789abcdef";

function set(
  sequence: number,
  objectId: string,
  payload: Record<string, unknown>,
): CanvasEvent {
  return {
    sequence,
    operationId: `op-${sequence}`,
    objectId,
    kind: "set",
    payload,
    correlationId,
  };
}

const create = (sequence: number, objectId: string) =>
  set(sequence, objectId, {
    op: "create",
    x: 10,
    y: 20,
    width: 100,
    height: 50,
    rotation: 0,
  });

describe("parseCanvasEvent", () => {
  it("parses a set and a delete frame", () => {
    expect(
      parseCanvasEvent(
        JSON.stringify({
          sequence: 3,
          operationId: "op-3",
          objectId: "box-1",
          kind: "set",
          payload: { op: "move", x: 1, y: 2 },
          correlationId,
        }),
      ),
    ).toEqual({
      sequence: 3,
      operationId: "op-3",
      objectId: "box-1",
      kind: "set",
      payload: { op: "move", x: 1, y: 2 },
      correlationId,
    });
    expect(
      parseCanvasEvent(
        JSON.stringify({
          sequence: 4,
          operationId: "op-4",
          objectId: "box-1",
          kind: "delete",
          payload: null,
          correlationId,
        }),
      ).payload,
    ).toBeNull();
  });

  it.each([
    ["not JSON", "nope"],
    [
      "a missing sequence",
      '{"operationId":"o","objectId":"b","kind":"set","payload":{},"correlationId":"c"}',
    ],
    [
      "an unknown kind",
      '{"sequence":1,"operationId":"o","objectId":"b","kind":"patch","payload":{},"correlationId":"c"}',
    ],
    [
      "a set without an object payload",
      '{"sequence":1,"operationId":"o","objectId":"b","kind":"set","payload":null,"correlationId":"c"}',
    ],
    [
      "a non-string operationId",
      '{"sequence":1,"operationId":7,"objectId":"b","kind":"set","payload":{},"correlationId":"c"}',
    ],
  ])("rejects %s", (_name, data) => {
    expect(() => parseCanvasEvent(data)).toThrow(/canvas event/);
  });
});

describe("reduceBoxes", () => {
  it("creates text boxes with empty content stacked by creation order", () => {
    const state = reduceBoxes(
      reduceBoxes(emptyBoxes, create(1, "a")),
      create(2, "b"),
    );

    expect(state.objects).toEqual([
      {
        id: "a",
        kind: "text",
        content: "",
        x: 10,
        y: 20,
        width: 100,
        height: 50,
        rotation: 0,
        z: 1,
      },
      {
        id: "b",
        kind: "text",
        content: "",
        x: 10,
        y: 20,
        width: 100,
        height: 50,
        rotation: 0,
        z: 2,
      },
    ]);
  });

  it("moves, resizes and rotates only the addressed box", () => {
    let state = reduceBoxes(
      reduceBoxes(emptyBoxes, create(1, "a")),
      create(2, "b"),
    );
    state = reduceBoxes(state, set(3, "a", { op: "move", x: 300, y: 400 }));
    state = reduceBoxes(
      state,
      set(4, "a", { op: "resize", width: 7, height: 8 }),
    );
    state = reduceBoxes(state, set(5, "a", { op: "rotate", rotation: 45 }));

    expect(state.objects).toEqual([
      {
        id: "a",
        kind: "text",
        content: "",
        x: 300,
        y: 400,
        width: 7,
        height: 8,
        rotation: 45,
        z: 1,
      },
      {
        id: "b",
        kind: "text",
        content: "",
        x: 10,
        y: 20,
        width: 100,
        height: 50,
        rotation: 0,
        z: 2,
      },
    ]);
  });

  it("removes a deleted box and keeps the creation order for later boxes", () => {
    let state = reduceBoxes(emptyBoxes, create(1, "a"));
    state = reduceBoxes(state, {
      sequence: 2,
      operationId: "op-2",
      objectId: "a",
      kind: "delete",
      payload: null,
      correlationId,
    });
    expect(state.objects).toEqual([]);

    state = reduceBoxes(state, create(3, "c"));
    expect(state.objects.map((object) => [object.id, object.z])).toEqual([
      ["c", 2],
    ]);
  });

  it.each([
    ["an unknown op", { op: "explode" }],
    ["a media op", { op: "media", action: "play" }],
    ["a move with a non-numeric coordinate", { op: "move", x: "1", y: 2 }],
  ])("ignores %s", (_name, payload) => {
    const before = reduceBoxes(emptyBoxes, create(1, "a"));

    expect(reduceBoxes(before, set(2, "a", payload))).toBe(before);
  });

  it("ignores an update to a box that does not exist", () => {
    const before = reduceBoxes(emptyBoxes, create(1, "a"));

    expect(reduceBoxes(before, set(2, "zz", { op: "move", x: 1, y: 1 }))).toBe(
      before,
    );
  });

  it.each(["constructor", "toString", "__proto__", "hasOwnProperty"])(
    "leaves the boxes unchanged for the inherited op %s without throwing",
    (op) => {
      const before = reduceBoxes(emptyBoxes, create(1, "a"));
      const event = set(2, "a", { op, x: 1, y: 1, rotation: 5 });

      expect(reduceBoxes(before, event)).toBe(before);
      expect(applyBoxEvent(before, event)).toEqual({
        boxes: before,
        changed: false,
      });
    },
  );
});

describe("applyBoxEvent", () => {
  const remove = (sequence: number, objectId: string): CanvasEvent => ({
    sequence,
    operationId: `op-${sequence}`,
    objectId,
    kind: "delete",
    payload: null,
    correlationId,
  });

  it.each([
    ["a delete of an unknown object", remove(2, "zz")],
    [
      "a create with invalid geometry",
      set(2, "b", { op: "create", x: 1, y: 2, width: "3", height: 4 }),
    ],
    ["an unknown op", set(2, "a", { op: "explode" })],
  ])("reports %s as no change", (_name, event) => {
    const before = reduceBoxes(emptyBoxes, create(1, "a"));

    expect(applyBoxEvent(before, event)).toEqual({
      boxes: before,
      changed: false,
    });
  });

  it.each([
    ["a valid create", create(2, "b")],
    ["a move", set(2, "a", { op: "move", x: 5, y: 6 })],
    ["a delete", remove(2, "a")],
  ])("reports %s as a change", (_name, event) => {
    const before = reduceBoxes(emptyBoxes, create(1, "a"));

    expect(applyBoxEvent(before, event)).toEqual({
      boxes: reduceBoxes(before, event),
      changed: true,
    });
  });
});

describe("mediaAction", () => {
  it("returns the action of a media event and nothing for others", () => {
    expect(mediaAction(set(1, "m", { op: "media", action: "play" }))).toBe(
      "play",
    );
    expect(mediaAction(set(1, "m", { op: "media", action: "pause" }))).toBe(
      "pause",
    );
    expect(
      mediaAction(set(1, "m", { op: "media", action: "stop" })),
    ).toBeUndefined();
    expect(mediaAction(create(1, "a"))).toBeUndefined();
  });
});
