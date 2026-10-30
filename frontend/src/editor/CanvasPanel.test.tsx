import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { CanvasPanel } from "./CanvasPanel";

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
  localStorage.clear();
  sessionStorage.clear();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const tenantId = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const canvasId = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

const session = {
  accountId: "0123456789abcdef0123456789abcdef",
  email: "alice@example.com",
  csrfToken: "csrf-token-value",
  operator: false,
};

function ownCanvas(width = 1920, height = 1080): Reply {
  return json(200, {
    tenant_id: tenantId,
    canvas_id: canvasId,
    width,
    height,
    revision: 0,
    objects: [
      {
        id: "cccccccccccccccccccccccccccccccc",
        kind: "text",
        content: "Far away",
        x: 2000,
        y: 1000,
        width: 200,
        height: 50,
        rotation: 0,
        z: 1,
      },
    ],
  });
}

function saved(width: number, height: number): Reply {
  return json(200, { canvas_id: canvasId, width, height, revision: 1 });
}

function renderPanel() {
  let next = 0;
  return render(
    <CanvasPanel
      session={session}
      newOperationId={() => `operation-id-${String(++next).padStart(4, "0")}`}
    />,
  );
}

function surface(): HTMLElement {
  const element = document.querySelector<HTMLElement>("[data-canvas-surface]");
  if (element === null) throw new Error("no canvas surface");
  return element;
}

function applySize(width: string, height: string) {
  fireEvent.change(screen.getByLabelText("Width"), {
    target: { value: width },
  });
  fireEvent.change(screen.getByLabelText("Height"), {
    target: { value: height },
  });
  fireEvent.click(screen.getByRole("button", { name: "Apply size" }));
}

async function ready() {
  await screen.findByText("1920 × 1080");
}

describe("CanvasPanel", () => {
  it("shows the own 1920 × 1080 canvas", async () => {
    replies.push(ownCanvas());
    renderPanel();

    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "Your canvas" }),
    ).toBeInTheDocument();
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/canvas" });
  });

  it("shows who holds each lock of the own canvas", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();
    expect(
      [...document.querySelectorAll("[data-lock-owners]")].map((element) =>
        element.getAttribute("data-lock-owners"),
      ),
    ).toEqual([`${tenantId}/${canvasId}`]);
  });

  it("draws the confirmed surface and prefills the size form", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    expect(surface().style.width).toBe("1920px");
    expect(surface().style.height).toBe("1080px");
    expect(surface()).toHaveTextContent("Far away");
    expect(
      screen.getByRole("form", { name: "Canvas size" }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText("Width")).toHaveValue("1920");
    expect(screen.getByLabelText("Height")).toHaveValue("1080");
    expect(document.body.textContent).not.toContain(tenantId);
    expect(document.body.textContent).not.toContain(canvasId);
    expect(document.body.innerHTML).not.toContain("cccccccc");
  });

  it("tells an account that is not active how to get activated", async () => {
    replies.push(
      json(403, {
        error: "forbidden",
        detail: "tenant cccc of eve@example.com",
      }),
    );
    renderPanel();

    const message = await screen.findByText(
      "Your account is not active yet. Ask an operator to activate it.",
    );
    expect(message).toHaveAttribute("role", "status");
    expect(document.body.textContent).not.toContain("eve@example.com");
    expect(document.body.textContent).not.toContain("cccc");
    expect(screen.queryByText(/×/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("form", { name: "Canvas size" }),
    ).not.toBeInTheDocument();
  });

  it("tells a deactivated streamer that the canvas and files are kept", async () => {
    replies.push(
      json(403, {
        error: "subscription_inactive",
        detail: "tenant cccc of eve@example.com",
      }),
    );
    renderPanel();

    const message = await screen.findByText(
      "Your subscription is inactive. Your canvas and files are kept; ask an operator to reactivate it.",
    );
    expect(message).toHaveAttribute("role", "status");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(
      screen.queryByText(
        "Your account is not active yet. Ask an operator to activate it.",
      ),
    ).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain("eve@example.com");
    expect(document.body.textContent).not.toContain("cccc");
    expect(screen.queryByText(/×/)).not.toBeInTheDocument();
    expect(
      screen.queryByRole("form", { name: "Canvas size" }),
    ).not.toBeInTheDocument();
  });

  it.each([json(500, { error: "internal", detail: "boom" }), "network-error"])(
    "shows the unavailable message for %o",
    async (reply) => {
      replies.push(reply as Reply);
      renderPanel();

      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Your canvas is unavailable. Reload the page to try again.",
      );
      expect(document.body.textContent).not.toContain("boom");
    },
  );

  const widthMessage = "Width must be a whole number from 320 to 3840.";
  const heightMessage = "Height must be a whole number from 180 to 2160.";

  it.each([
    ["", "1080", widthMessage],
    ["1.5", "1080", widthMessage],
    ["abc", "1080", widthMessage],
    ["319", "1080", widthMessage],
    ["3841", "1080", widthMessage],
    ["1920", "", heightMessage],
    ["1920", "1.5", heightMessage],
    ["1920", "abc", heightMessage],
    ["1920", "179", heightMessage],
    ["1920", "2161", heightMessage],
  ])("rejects %o × %o without a request", async (width, height, message) => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    applySize(width, height);

    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(calls).toHaveLength(1);
    expect(screen.getByText("1920 × 1080")).toBeInTheDocument();
    expect(surface().style.width).toBe("1920px");
    expect(surface().style.height).toBe("1080px");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("names both fields when both are invalid", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    applySize("0", "9999");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(widthMessage);
    expect(alert).toHaveTextContent(heightMessage);
    expect(calls).toHaveLength(1);
  });

  it.each([
    ["320", "180", 320, 180],
    ["3840", "2160", 3840, 2160],
  ])(
    "saves the boundary %s × %s and shows the confirmed size",
    async (width, height, sentWidth, sentHeight) => {
      replies.push(ownCanvas());
      renderPanel();
      await ready();

      replies.push(saved(sentWidth, sentHeight));
      applySize(width, height);

      expect(await screen.findByRole("status")).toHaveTextContent(
        "Canvas size saved.",
      );
      expect(calls[1]).toMatchObject({
        method: "PUT",
        path: `/api/tenants/${tenantId}/canvases/${canvasId}/dimensions`,
        headers: { "X-CSRF-Token": "csrf-token-value" },
        body: {
          operation_id: "operation-id-0001",
          width: sentWidth,
          height: sentHeight,
        },
      });
      expect(
        screen.getByText(`${sentWidth} × ${sentHeight}`),
      ).toBeInTheDocument();
      expect(surface().style.width).toBe(`${sentWidth}px`);
      expect(surface().style.height).toBe(`${sentHeight}px`);
      // The object outside the smaller area stays on the surface.
      expect(surface()).toHaveTextContent("Far away");
    },
  );

  it("shows the size the server confirmed, not the one typed", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    replies.push(saved(1280, 720));
    applySize("1281", "721");

    await screen.findByText("Canvas size saved.");
    expect(screen.getByText("1280 × 720")).toBeInTheDocument();
    expect(surface().style.width).toBe("1280px");
    expect(screen.getByLabelText("Width")).toHaveValue("1280");
    expect(screen.getByLabelText("Height")).toHaveValue("720");
  });

  it.each([
    ["bad_request", 400, "That size is not allowed."],
    ["forbidden", 403, "Only the canvas owner can change its size."],
    ["conflict", 409, "That change was already used. Try again."],
    ["unauthorized", 401, "The size could not be saved. Try again."],
    ["internal", 500, "The size could not be saved. Try again."],
    ["something_new", 500, "The size could not be saved. Try again."],
  ])("maps %s to a fixed message", async (code, status, message) => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    replies.push(
      json(status, {
        error: code,
        detail: "account 2222 of mallory@example.com",
        correlation_id: "corr-3333",
      }),
    );
    applySize("1280", "720");

    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(screen.getByText("1920 × 1080")).toBeInTheDocument();
    expect(surface().style.width).toBe("1920px");
    expect(document.body.textContent).not.toContain("mallory@example.com");
    expect(document.body.textContent).not.toContain("2222");
    expect(document.body.textContent).not.toContain("corr-3333");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("shows a network failure as a fixed message", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    replies.push("network-error");
    applySize("1280", "720");

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The size could not be saved. Try again.",
    );
  });

  it("retries with the same operation ID until it gets an answer", async () => {
    replies.push(ownCanvas());
    renderPanel();
    await ready();

    replies.push("network-error");
    applySize("1280", "720");
    await screen.findByRole("alert");
    replies.push(json(500, { error: "internal" }));
    fireEvent.click(screen.getByRole("button", { name: "Apply size" }));
    await waitFor(() => expect(calls).toHaveLength(3));
    await waitFor(() =>
      expect(
        screen.getByRole("button", { name: "Apply size" }),
      ).not.toBeDisabled(),
    );
    expect(calls[1].body?.operation_id).toBe("operation-id-0001");
    expect(calls[2].body?.operation_id).toBe("operation-id-0001");

    // Changed values are a new intent with a new operation ID.
    replies.push(json(400, { error: "bad_request" }));
    applySize("1280", "800");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "That size is not allowed.",
    );
    expect(calls[3].body?.operation_id).toBe("operation-id-0002");

    // After a definitive answer, even the same values start a new operation.
    replies.push(saved(1280, 800));
    fireEvent.click(screen.getByRole("button", { name: "Apply size" }));
    await screen.findByText("Canvas size saved.");
    expect(calls[4].body?.operation_id).toBe("operation-id-0003");

    replies.push(saved(1280, 800));
    applySize("1280", "800");
    await waitFor(() => expect(calls).toHaveLength(6));
    expect(calls[5].body?.operation_id).toBe("operation-id-0004");
  });

  it("refetches the confirmed size on remount and caches nothing", async () => {
    replies.push(ownCanvas());
    const first = renderPanel();
    await ready();
    replies.push(saved(1280, 720));
    applySize("1280", "720");
    await screen.findByText("1280 × 720");
    first.unmount();

    // Another tab changed the size meanwhile; the server answer wins.
    replies.push(ownCanvas(2560, 1440));
    renderPanel();

    expect(await screen.findByText("2560 × 1440")).toBeInTheDocument();
    expect(calls).toHaveLength(3);
    expect(calls[2]).toMatchObject({ method: "GET", path: "/api/canvas" });
    expect(surface().style.width).toBe("2560px");
    expect(surface().style.height).toBe("1440px");
    expect(screen.getByLabelText("Width")).toHaveValue("2560");
    expect(localStorage).toHaveLength(0);
    expect(sessionStorage).toHaveLength(0);
  });

  it("draws an image object from its content route", async () => {
    replies.push(
      json(200, {
        tenant_id: tenantId,
        canvas_id: canvasId,
        width: 1920,
        height: 1080,
        revision: 1,
        objects: [
          {
            id: "dddddddddddddddddddddddddddddddd",
            kind: "image",
            content: "reference-1",
            x: 0,
            y: 0,
            width: 640,
            height: 480,
            rotation: 0,
            z: 0,
          },
        ],
      }),
    );
    renderPanel();
    await ready();

    const image = surface().querySelector("img");
    expect(image).toHaveAttribute(
      "src",
      `/api/tenants/${tenantId}/canvases/${canvasId}/media/references/reference-1/content`,
    );
    expect(calls).toHaveLength(1);
  });

  it("loads the canvas again when asked to refresh", async () => {
    replies.push(ownCanvas());
    const { rerender } = render(
      <CanvasPanel session={session} refreshKey={0} />,
    );
    await ready();

    replies.push(
      json(200, {
        tenant_id: tenantId,
        canvas_id: canvasId,
        width: 1920,
        height: 1080,
        revision: 2,
        objects: [
          {
            id: "dddddddddddddddddddddddddddddddd",
            kind: "image",
            content: "reference-2",
            x: 0,
            y: 0,
            width: 64,
            height: 64,
            rotation: 0,
            z: 2,
          },
        ],
      }),
    );
    rerender(<CanvasPanel session={session} refreshKey={1} />);

    await waitFor(() =>
      expect(surface().querySelector("img")).toHaveAttribute(
        "src",
        `/api/tenants/${tenantId}/canvases/${canvasId}/media/references/reference-2/content`,
      ),
    );
    expect(calls.map((call) => [call.method, call.path])).toEqual([
      ["GET", "/api/canvas"],
      ["GET", "/api/canvas"],
    ]);
    expect(surface()).not.toHaveTextContent("Far away");
  });
});
