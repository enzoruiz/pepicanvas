import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
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
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/");
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

const noMemberships = json(200, { memberships: [] });

const token = `pci_${"t".repeat(43)}`;
const signInNote =
  "Sign in or create an account to accept the moderator invitation.";
const moderated = {
  tenant_id: "cccccccccccccccccccccccccccccccc",
  canvas_id: "dddddddddddddddddddddddddddddddd",
};
const moderatedCanvas = json(200, {
  canvas_id: moderated.canvas_id,
  width: 1280,
  height: 720,
  revision: 2,
  objects: [],
});
const oneMembership = json(200, {
  memberships: [{ ...moderated, role: "moderator" }],
});
const forbidden = json(403, { error: "forbidden" });

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
    replies.push(session(false), ownCanvas, noMemberships);
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

  it("shows an active owner the invitations, moderators, and storage panels", async () => {
    replies.push(
      session(false),
      ownCanvas,
      noMemberships,
      json(200, { invitations: [] }),
      json(200, { members: [] }),
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
    expect(
      screen.getByRole("region", { name: "Moderators" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("No moderators yet.")).toBeInTheDocument();
    await waitFor(() => expect(calls).toHaveLength(6));
    expect(calls[3]).toMatchObject({ method: "GET", path: "/api/invitations" });
    expect(calls[4]).toMatchObject({ method: "GET", path: "/api/members" });
    expect(calls[5]).toMatchObject({
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
        noMemberships,
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
      expect(
        screen.queryByRole("region", { name: "Moderators" }),
      ).not.toBeInTheDocument();
      await waitFor(() => expect(calls).toHaveLength(3));
      expect(calls[2]).toMatchObject({
        method: "GET",
        path: "/api/memberships",
      });
    },
  );

  it("shows an operator the operator panel", async () => {
    replies.push(session(true), forbidden, noMemberships);
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

  it("shows only sign-in and a note for an invitation link without a session", async () => {
    window.history.replaceState(null, "", `/editor/#${token}`);
    replies.push(json(401, { error: "unauthorized" }));
    render(<EditorShell />);

    expect(
      await screen.findByRole("heading", { level: 2, name: "Sign in" }),
    ).toBeInTheDocument();
    expect(screen.getByText(signInNote)).toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls.map((call) => call.path)).toEqual(["/api/session"]);
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("region", { name: "Your canvas" }),
    ).not.toBeInTheDocument();
    expect(document.querySelector("[data-canvas-surface]")).toBeNull();
    expect(document.body.textContent).not.toContain("pci_");
    // Signing in does not navigate, so the fragment is still there.
    expect(window.location.hash).toBe(`#${token}`);
  });

  it("shows no invitation note without a link", async () => {
    replies.push(json(401, { error: "unauthorized" }));
    render(<EditorShell />);

    await screen.findByRole("heading", { level: 2, name: "Sign in" });
    expect(screen.queryByText(signInNote)).not.toBeInTheDocument();
  });

  it("accepts an invitation after an explicit click and shows the moderated canvas", async () => {
    window.history.replaceState(null, "", `/editor/#${token}`);
    replies.push(session(false), forbidden, noMemberships);
    render(<EditorShell />);

    expect(
      await screen.findByText(
        "You were invited to moderate a streamer's canvas.",
      ),
    ).toBeInTheDocument();
    await waitFor(() => expect(calls).toHaveLength(3));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls.map((call) => call.path)).not.toContain(
      "/api/invitations/accept",
    );

    replies.push(json(200, moderated), oneMembership, moderatedCanvas);
    fireEvent.click(screen.getByRole("button", { name: "Accept invitation" }));

    expect(
      await screen.findByText("You are now a moderator of this canvas."),
    ).toBeInTheDocument();
    expect(await screen.findByText("1280 × 720")).toBeInTheDocument();
    expect(calls.slice(3).map((call) => [call.method, call.path])).toEqual([
      ["POST", "/api/invitations/accept"],
      ["GET", "/api/memberships"],
      [
        "GET",
        `/api/tenants/${moderated.tenant_id}/canvases/${moderated.canvas_id}`,
      ],
    ]);
    expect(window.location.hash).toBe("");
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("region", { name: "Invite moderators" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Width")).not.toBeInTheDocument();
  });

  it("shows a moderator its moderated canvas after a reload without a link", async () => {
    replies.push(session(false), forbidden, oneMembership, moderatedCanvas);
    render(<EditorShell />);

    expect(
      await screen.findByRole("region", { name: "Canvases you moderate" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("1280 × 720")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Width")).not.toBeInTheDocument();
    // A moderator who owns nothing is not told its account is inactive.
    expect(
      screen.queryByRole("region", { name: "Your canvas" }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/not active yet/)).not.toBeInTheDocument();
  });
});
