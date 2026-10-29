import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
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

const endedMessage = "Your moderator access to this canvas has ended.";

function forbidden(): Reply {
  return json(403, {
    error: "forbidden",
    detail: `tenant ${first.tenant} canvas ${first.canvas} bob@example.com`,
  });
}

function moderatedCanvases() {
  return screen.queryAllByRole("article", { name: "Moderated canvas" });
}

function refresh(index: number) {
  fireEvent.click(
    within(moderatedCanvases()[index]).getByRole("button", {
      name: "Refresh canvas",
    }),
  );
}

function expectNoSecrets() {
  const text = document.body.textContent ?? "";
  for (const secret of [
    first.tenant,
    first.canvas,
    second.tenant,
    "bob@example.com",
  ]) {
    expect(text).not.toContain(secret);
  }
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
      json(500, { error: "internal" }),
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

  it("drops a canvas whose access ended while the list was read", async () => {
    const onListed = vi.fn();
    replies.push(
      memberships(first, second),
      forbidden(),
      canvas(second.canvas, 800, 600),
    );
    render(<ModeratedCanvases onListed={onListed} />);

    expect(await screen.findByRole("alert")).toHaveTextContent(endedMessage);
    expect(screen.getByText("800 × 600")).toBeInTheDocument();
    expect(moderatedCanvases()).toHaveLength(1);
    expect(onListed).toHaveBeenLastCalledWith(1);
    expectNoSecrets();
  });

  it("refreshes a canvas on request", async () => {
    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    render(<ModeratedCanvases />);
    await screen.findByText("1280 × 720");

    replies.push(canvas(first.canvas, 1920, 1080));
    refresh(0);

    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
    expect(screen.queryByText("1280 × 720")).not.toBeInTheDocument();
    expect(calls.map((call) => call.path)).toEqual([
      "/api/memberships",
      canvasPath(first),
      canvasPath(first),
    ]);
  });

  it("stops showing a refreshed canvas that cannot be loaded", async () => {
    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    render(<ModeratedCanvases />);
    await screen.findByText("1280 × 720");

    replies.push(json(500, { error: "internal" }));
    refresh(0);

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This canvas is unavailable. Reload the page to try again.",
    );
    expect(screen.queryByText("1280 × 720")).not.toBeInTheDocument();
    expect(document.querySelector("[data-canvas-surface]")).toBeNull();
    expect(calls).toHaveLength(3);
  });

  it("shows a safe reason and lists again when a canvas read is refused", async () => {
    const onListed = vi.fn();
    replies.push(
      memberships(first, second),
      canvas(first.canvas, 1280, 720),
      canvas(second.canvas, 800, 600),
    );
    render(<ModeratedCanvases onListed={onListed} />);
    await screen.findByText("1280 × 720");
    await screen.findByText("800 × 600");

    replies.push(
      forbidden(),
      memberships(second),
      canvas(second.canvas, 800, 600),
    );
    refresh(0);

    expect(await screen.findByRole("alert")).toHaveTextContent(endedMessage);
    expect(screen.queryByText("1280 × 720")).not.toBeInTheDocument();
    expect(screen.queryByText("Text on 1280")).not.toBeInTheDocument();
    await waitFor(() => expect(calls).toHaveLength(6));
    expect(calls.slice(3).map((call) => call.path)).toEqual([
      canvasPath(first),
      "/api/memberships",
      canvasPath(second),
    ]);
    await waitFor(() => expect(onListed).toHaveBeenLastCalledWith(1));
    expect(moderatedCanvases()).toHaveLength(1);
    expect(screen.getByText("800 × 600")).toBeInTheDocument();
    expectNoSecrets();
    expect(window.localStorage).toHaveLength(0);
    expect(window.sessionStorage).toHaveLength(0);
  });

  it("keeps the reason after the last moderated canvas is gone", async () => {
    const onListed = vi.fn();
    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    render(<ModeratedCanvases onListed={onListed} />);
    await screen.findByText("1280 × 720");

    replies.push(forbidden(), memberships());
    refresh(0);

    expect(await screen.findByRole("alert")).toHaveTextContent(endedMessage);
    await waitFor(() => expect(onListed).toHaveBeenLastCalledWith(0));
    expect(
      screen.getByRole("region", { name: "Canvases you moderate" }),
    ).toBeInTheDocument();
    expect(document.querySelector("[data-canvas-surface]")).toBeNull();
    expect(screen.queryByText("Text on 1280")).not.toBeInTheDocument();
    expect(calls).toHaveLength(4);
    expectNoSecrets();
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
