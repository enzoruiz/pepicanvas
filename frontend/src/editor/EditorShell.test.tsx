import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { EditorShell } from "./EditorShell";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function session(operator: boolean) {
  return json(200, {
    account_id: "0123456789abcdef0123456789abcdef",
    email: "alice@example.com",
    csrf_token: "csrf-token-value",
    operator,
  });
}

const ownCanvas = json(200, {
  tenant_id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  canvas_id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  width: 1920,
  height: 1080,
  revision: 0,
  objects: [],
});

describe("EditorShell", () => {
  it("renders the editor mount boundary", async () => {
    replies.push(json(401, { error: "unauthorized" }));
    render(<EditorShell />);

    expect(
      screen.getByRole("main", { name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", { level: 2, name: "Sign in" }),
    ).toBeInTheDocument();
    expect(calls).toHaveLength(1);
  });

  it("shows a streamer its canvas and no operator panel", async () => {
    replies.push(session(false), ownCanvas);
    render(<EditorShell />);

    expect(await screen.findByText("1920 × 1080")).toBeInTheDocument();
    expect(calls[1]).toMatchObject({ method: "GET", path: "/api/canvas" });
    expect(
      screen.queryByRole("region", { name: "Manage streamers" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Activate" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Deactivate" }),
    ).not.toBeInTheDocument();
  });

  it("shows an active owner the invitations and storage panels", async () => {
    replies.push(
      session(false),
      ownCanvas,
      json(200, { invitations: [] }),
      json(200, {
        quota_bytes: 1_000_000_000,
        used_bytes: 0,
        reserved_bytes: 0,
        available_bytes: 1_000_000_000,
      }),
    );
    render(<EditorShell />);

    expect(
      await screen.findByRole("region", { name: "Invite moderators" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByText("Used 0 of 1,000,000,000 bytes"),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("region", { name: "Canvas storage" }),
    ).toBeInTheDocument();
    expect(calls).toHaveLength(4);
    expect(calls[2]).toMatchObject({ method: "GET", path: "/api/invitations" });
    expect(calls[3]).toMatchObject({
      method: "GET",
      path: "/api/tenants/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/canvases/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb/media/usage",
    });
  });

  it.each([
    ["an inactive account or moderator", false, 403],
    ["an operator-only account", true, 403],
    ["an account whose canvas is unavailable", false, 500],
  ])(
    "hides the invitations and storage panels from %s",
    async (_, operator, status) => {
      replies.push(
        session(operator),
        json(status, { error: status === 403 ? "forbidden" : "internal" }),
      );
      render(<EditorShell />);

      expect(
        await screen.findByText(
          status === 403
            ? "Your account is not active yet. Ask an operator to activate it."
            : "Your canvas is unavailable. Reload the page to try again.",
        ),
      ).toBeInTheDocument();
      expect(
        screen.queryByRole("region", { name: "Invite moderators" }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole("button", { name: "Create invitation link" }),
      ).not.toBeInTheDocument();
      expect(
        screen.queryByRole("region", { name: "Canvas storage" }),
      ).not.toBeInTheDocument();
      expect(calls).toHaveLength(2);
    },
  );

  it("shows an operator the operator panel", async () => {
    replies.push(session(true), json(403, { error: "forbidden" }));
    render(<EditorShell />);

    expect(
      await screen.findByRole("region", { name: "Manage streamers" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByText(
        "Your account is not active yet. Ask an operator to activate it.",
      ),
    ).toBeInTheDocument();
  });
});
