import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { CanvasPanel } from "./CanvasPanel";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("CanvasPanel", () => {
  it("shows the own 1920 × 1080 canvas", async () => {
    replies.push(
      json(200, {
        tenant_id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
        canvas_id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
        width: 1920,
        height: 1080,
        revision: 0,
        objects: [],
      }),
    );
    render(<CanvasPanel />);

    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "Your canvas" }),
    ).toBeInTheDocument();
    expect(calls).toHaveLength(1);
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/canvas" });
  });

  it("tells an account that is not active how to get activated", async () => {
    replies.push(
      json(403, {
        error: "forbidden",
        detail: "tenant cccc of eve@example.com",
      }),
    );
    render(<CanvasPanel />);

    const message = await screen.findByText(
      "Your account is not active yet. Ask an operator to activate it.",
    );
    expect(message).toHaveAttribute("role", "status");
    expect(document.body.textContent).not.toContain("eve@example.com");
    expect(document.body.textContent).not.toContain("cccc");
    expect(screen.queryByText(/×/)).not.toBeInTheDocument();
  });

  it("tells a deactivated streamer that the canvas and files are kept", async () => {
    replies.push(
      json(403, {
        error: "subscription_inactive",
        detail: "tenant cccc of eve@example.com",
      }),
    );
    render(<CanvasPanel />);

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
  });

  it.each([json(500, { error: "internal", detail: "boom" }), "network-error"])(
    "shows the unavailable message for %o",
    async (reply) => {
      replies.push(reply as Reply);
      render(<CanvasPanel />);

      expect(await screen.findByRole("alert")).toHaveTextContent(
        "Your canvas is unavailable. Reload the page to try again.",
      );
      expect(document.body.textContent).not.toContain("boom");
    },
  );
});
