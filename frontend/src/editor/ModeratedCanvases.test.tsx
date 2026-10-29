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

// The lock holders poll their own route; their behavior is tested in
// locks/LockOwners.test.tsx.
vi.mock("../locks/LockOwners", () => ({
  LockOwners: ({
    tenantId,
    canvasId,
  }: {
    tenantId: string;
    canvasId: string;
  }) => <p data-lock-owners={`${tenantId}/${canvasId}`} />,
}));

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

const endedMessage =
  "Your moderator access to a canvas has ended. It was removed from this list.";
const refreshFailedMessage =
  "This canvas could not be refreshed. The last loaded version is shown. Try again.";

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

  it("shows who holds each lock of every moderated canvas", async () => {
    replies.push(
      memberships(first, second),
      canvas(first.canvas, 1280, 720),
      canvas(second.canvas, 800, 600),
    );
    render(<ModeratedCanvases />);
    await screen.findByText("800 × 600");
    expect(
      [...document.querySelectorAll("[data-lock-owners]")].map((element) =>
        element.getAttribute("data-lock-owners"),
      ),
    ).toEqual([
      `${first.tenant}/${first.canvas}`,
      `${second.tenant}/${second.canvas}`,
    ]);
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

  it.each([
    ["a server error", json(500, { error: "internal" })],
    ["a network error", "network-error" as const],
  ])(
    "keeps the last loaded canvas when a refresh fails with %s",
    async (_, failure) => {
      replies.push(memberships(first), canvas(first.canvas, 1280, 720));
      render(<ModeratedCanvases />);
      await screen.findByText("1280 × 720");

      replies.push(failure);
      refresh(0);

      expect(await screen.findByRole("alert")).toHaveTextContent(
        refreshFailedMessage,
      );
      expect(screen.getByText("1280 × 720")).toBeInTheDocument();
      expect(screen.getByText("Text on 1280")).toBeInTheDocument();
      expect(calls).toHaveLength(3);

      replies.push(canvas(first.canvas, 1920, 1080));
      refresh(0);

      expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    },
  );

  it("keeps the fixed message for a canvas that never loaded and retries it", async () => {
    replies.push(memberships(first), json(500, { error: "internal" }));
    render(<ModeratedCanvases />);
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This canvas is unavailable. Reload the page to try again.",
    );

    replies.push(json(500, { error: "internal" }));
    refresh(0);
    await waitFor(() => expect(calls).toHaveLength(3));
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This canvas is unavailable. Reload the page to try again.",
    );
    expect(document.querySelector("[data-canvas-surface]")).toBeNull();
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

  it("clears the ended reason when the list is loaded again with every canvas readable", async () => {
    replies.push(
      memberships(first, second),
      forbidden(),
      canvas(second.canvas, 800, 600),
    );
    const { rerender } = render(<ModeratedCanvases refreshKey={0} />);
    expect(await screen.findByRole("alert")).toHaveTextContent(endedMessage);

    replies.push(memberships(second), canvas(second.canvas, 800, 600));
    rerender(<ModeratedCanvases refreshKey={1} />);

    await waitFor(() => expect(calls).toHaveLength(5));
    await waitFor(() =>
      expect(screen.queryByRole("alert")).not.toBeInTheDocument(),
    );
    expect(screen.getByText("800 × 600")).toBeInTheDocument();
  });

  it("disables a canvas's refresh while its read is in flight", async () => {
    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    render(<ModeratedCanvases />);
    await screen.findByText("1280 × 720");

    const deferred = deferNextFetch();
    refresh(0);
    const button = within(moderatedCanvases()[0]).getByRole("button", {
      name: "Refresh canvas",
    });
    expect(button).toBeDisabled();
    fireEvent.click(button);
    expect(calls).toHaveLength(3);

    deferred.resolve(canvas(first.canvas, 1920, 1080));
    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
    expect(button).toBeEnabled();
  });

  it("ignores a refresh answer that arrives after a newer list load", async () => {
    replies.push(memberships(first), canvas(first.canvas, 1280, 720));
    const { rerender } = render(<ModeratedCanvases refreshKey={0} />);
    await screen.findByText("1280 × 720");

    const stale = deferNextFetch();
    refresh(0);
    replies.push(memberships(first), canvas(first.canvas, 1920, 1080));
    rerender(<ModeratedCanvases refreshKey={1} />);
    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();

    stale.resolve(canvas(first.canvas, 640, 360));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(screen.getByText("1920 × 1080")).toBeInTheDocument();
    expect(screen.queryByText("640 × 360")).not.toBeInTheDocument();
  });
});

// deferNextFetch makes the next request wait until the test answers it,
// leaving later requests to the reply queue.
function deferNextFetch() {
  const queued = globalThis.fetch;
  let answer: (reply: Reply) => void = () => undefined;
  const pending = new Promise<Reply>((resolve) => {
    answer = resolve;
  });
  let used = false;
  vi.stubGlobal(
    "fetch",
    vi.fn((path: string, init: RequestInit) => {
      if (used) return queued(path, init);
      used = true;
      calls.push({
        method: init.method ?? "GET",
        path,
        headers: init.headers as Record<string, string>,
        body: undefined,
      });
      return pending.then((reply) =>
        reply === "network-error"
          ? Promise.reject(new TypeError("Failed to fetch"))
          : new Response(
              reply.body === undefined ? null : JSON.stringify(reply.body),
              { status: reply.status },
            ),
      );
    }),
  );
  return {
    resolve: (reply: Reply) => {
      answer(reply);
    },
  };
}
