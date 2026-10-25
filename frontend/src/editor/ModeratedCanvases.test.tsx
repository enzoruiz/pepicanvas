import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { ModeratedCanvases } from "./ModeratedCanvases";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const first = {
  tenant: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  canvas: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
};
const second = {
  tenant: "cccccccccccccccccccccccccccccccc",
  canvas: "dddddddddddddddddddddddddddddddd",
};

function memberships(...entries: (typeof first)[]): Reply {
  return json(200, {
    memberships: entries.map(({ tenant, canvas }) => ({
      tenant_id: tenant,
      canvas_id: canvas,
      role: "moderator",
    })),
  });
}

function canvas(id: string, width: number, height: number): Reply {
  return json(200, {
    canvas_id: id,
    width,
    height,
    revision: 1,
    objects: [
      {
        id: "obj-1",
        kind: "text",
        content: `Text on ${width}`,
        x: 0,
        y: 0,
        width: 100,
        height: 20,
        rotation: 0,
        z: 0,
      },
    ],
  });
}

function canvasPath({ tenant, canvas }: typeof first) {
  return `/api/tenants/${tenant}/canvases/${canvas}`;
}

function expectReadOnly() {
  expect(screen.queryByLabelText("Width")).not.toBeInTheDocument();
  expect(screen.queryByLabelText("Height")).not.toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Apply size" }),
  ).not.toBeInTheDocument();
  expect(
    screen.queryByRole("button", { name: "Create invitation link" }),
  ).not.toBeInTheDocument();
  expect(
    screen.queryByRole("region", { name: "Invite moderators" }),
  ).not.toBeInTheDocument();
  expect(
    screen.queryByRole("region", { name: "Manage streamers" }),
  ).not.toBeInTheDocument();
}

describe("ModeratedCanvases", () => {
  it("renders nothing for an account that moderates no canvas", async () => {
    replies.push(memberships());
    const { container } = render(<ModeratedCanvases />);

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/memberships" });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(container).toBeEmptyDOMElement();
    expect(calls).toHaveLength(1);
  });

  it("shows every moderated canvas read-only", async () => {
    replies.push(
      memberships(first, second),
      canvas(first.canvas, 1280, 720),
      canvas(second.canvas, 800, 600),
    );
    render(<ModeratedCanvases />);

    expect(
      await screen.findByRole("region", { name: "Canvases you moderate" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("1280 × 720")).toBeInTheDocument();
    expect(await screen.findByText("800 × 600")).toBeInTheDocument();
    expect(screen.getByText("Text on 1280")).toBeInTheDocument();
    expect(document.querySelectorAll("[data-canvas-surface]")).toHaveLength(2);
    expect(calls.map((call) => [call.method, call.path])).toEqual([
      ["GET", "/api/memberships"],
      ["GET", canvasPath(first)],
      ["GET", canvasPath(second)],
    ]);
    expectReadOnly();
    expect(document.body.textContent).not.toContain(first.tenant);
    expect(document.body.textContent).not.toContain(first.canvas);
  });

  it("shows a fixed message for a canvas that cannot be loaded", async () => {
    replies.push(
      memberships(first, second),
      json(403, { error: "forbidden" }),
      canvas(second.canvas, 800, 600),
    );
    render(<ModeratedCanvases />);

    expect(await screen.findByText("800 × 600")).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This canvas is unavailable. Reload the page to try again.",
    );
  });

  it("shows a fixed message when the list cannot be loaded", async () => {
    replies.push(json(500, { error: "internal", detail: "secret" }));
    render(<ModeratedCanvases />);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The canvases you moderate are unavailable. Reload the page to try again.",
    );
    expect(document.body.textContent).not.toContain("secret");
    expectReadOnly();
  });

  it("loads the list again when asked to refresh", async () => {
    replies.push(memberships());
    const { rerender } = render(<ModeratedCanvases refreshKey={0} />);
    await waitFor(() => expect(calls).toHaveLength(1));

    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    rerender(<ModeratedCanvases refreshKey={1} />);

    expect(await screen.findByText("1280 × 720")).toBeInTheDocument();
    expect(calls.map((call) => call.path)).toEqual([
      "/api/memberships",
      "/api/memberships",
      canvasPath(first),
    ]);
  });
});
