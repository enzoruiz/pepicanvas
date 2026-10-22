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
import { InvitationsPanel } from "./InvitationsPanel";

let calls: Call[];
let replies: Reply[];
let writeText: ReturnType<typeof vi.fn>;

beforeEach(() => {
  ({ calls, replies } = stubFetch());
  writeText = vi.fn(() => Promise.resolve());
  Object.defineProperty(navigator, "clipboard", {
    configurable: true,
    value: { writeText },
  });
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const session = {
  accountId: "0123456789abcdef0123456789abcdef",
  email: "alice@example.com",
  csrfToken: "csrf-token-value",
  operator: false,
};

const firstId = "11111111111111111111111111111111";
const secondId = "22222222222222222222222222222222";

function token(n: number) {
  return `pci_${String(n).padStart(43, "x")}`;
}

function invitation(id: string, expiresAt = "2026-10-29T10:00:00Z") {
  return {
    invitation_id: id,
    created_at: "2026-10-22T10:00:00Z",
    expires_at: expiresAt,
  };
}

function list(...ids: string[]): Reply {
  return json(200, { invitations: ids.map((id) => invitation(id)) });
}

function renderPanel() {
  let nextOperation = 0;
  let nextToken = 0;
  return render(
    <InvitationsPanel
      session={session}
      newOperationId={() =>
        `operation-id-${String(++nextOperation).padStart(4, "0")}`
      }
      newToken={() => token(++nextToken)}
    />,
  );
}

const linkNote = "This link is shown only once. It expires in 7 days.";

function linkField() {
  return screen.queryByLabelText("Invitation link");
}

function createLink() {
  fireEvent.click(
    screen.getByRole("button", { name: "Create invitation link" }),
  );
}

describe("InvitationsPanel", () => {
  it("lists the pending invitations with their expiry and a revoke button", async () => {
    replies.push(list(firstId, secondId));
    renderPanel();

    expect(
      screen.getByRole("heading", { level: 2, name: "Invite moderators" }),
    ).toBeInTheDocument();
    const rows = await screen.findAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/invitations" });
    expect(rows[0].querySelector("time")).toHaveAttribute(
      "dateTime",
      "2026-10-29T10:00:00Z",
    );
    expect(rows[0]).toHaveTextContent(/^Expires /);
    expect(
      within(rows[0]).getByRole("button", { name: "Revoke" }),
    ).toBeInTheDocument();
    expect(document.body.textContent).not.toContain(firstId);
    expect(linkField()).not.toBeInTheDocument();
  });

  it("says when there are no pending invitations", async () => {
    replies.push(list());
    renderPanel();

    expect(
      await screen.findByText("No pending invitations."),
    ).toBeInTheDocument();
  });

  it("creates an invitation and shows its fragment link once", async () => {
    replies.push(list());
    renderPanel();
    await screen.findByText("No pending invitations.");

    replies.push(json(201, invitation(firstId)), list(firstId));
    createLink();

    const field = await screen.findByLabelText("Invitation link");
    const link = `${window.location.origin}/editor/#${token(1)}`;
    expect(field).toHaveValue(link);
    expect(field).toHaveAttribute("readonly");
    expect(screen.getByText(linkNote)).toBeInTheDocument();
    expect(calls[1]).toEqual({
      method: "POST",
      path: "/api/invitations",
      headers: {
        "X-CSRF-Token": "csrf-token-value",
        "Content-Type": "application/json",
      },
      body: { operation_id: "operation-id-0001", token: token(1) },
    });
    for (const call of calls) {
      expect(call.path).not.toContain("pci_");
    }
    // The list reloads; the token appears only in the field.
    await waitFor(() => expect(calls).toHaveLength(3));
    expect(calls[2]).toMatchObject({ method: "GET", path: "/api/invitations" });
    expect(await screen.findAllByRole("listitem")).toHaveLength(1);
    expect(document.body.textContent).not.toContain("pci_");

    fireEvent.click(screen.getByRole("button", { name: "Copy link" }));
    expect(writeText).toHaveBeenCalledWith(link);
    expect(await screen.findByText("Link copied.")).toBeInTheDocument();
  });

  it("clears the shown link when the panel reloads", async () => {
    replies.push(list());
    const view = renderPanel();
    await screen.findByText("No pending invitations.");
    replies.push(json(201, invitation(firstId)), list(firstId));
    createLink();
    await screen.findByLabelText("Invitation link");
    await waitFor(() => expect(calls).toHaveLength(3));

    view.unmount();
    replies.push(list(firstId));
    renderPanel();

    expect(await screen.findAllByRole("listitem")).toHaveLength(1);
    expect(linkField()).not.toBeInTheDocument();
    expect(screen.queryByText(linkNote)).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain("pci_");
  });

  it("clears the shown link when another link is created", async () => {
    replies.push(list());
    renderPanel();
    await screen.findByText("No pending invitations.");
    replies.push(json(201, invitation(firstId)), list(firstId));
    createLink();
    await screen.findByLabelText("Invitation link");
    await waitFor(() => expect(calls).toHaveLength(3));

    replies.push("network-error");
    createLink();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The service is unavailable. Try again.",
    );
    expect(linkField()).not.toBeInTheDocument();
    expect(document.body.innerHTML).not.toContain("pci_");
  });

  it("retries with the same operation and token until it gets an answer", async () => {
    replies.push(list());
    renderPanel();
    await screen.findByText("No pending invitations.");

    replies.push("network-error");
    createLink();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The service is unavailable. Try again.",
    );
    replies.push(json(500, { error: "internal" }));
    createLink();
    await waitFor(() => expect(calls).toHaveLength(3));
    expect(calls[1].body).toEqual({
      operation_id: "operation-id-0001",
      token: token(1),
    });
    expect(calls[2].body).toEqual(calls[1].body);

    // A definitive answer, even a refusal, starts a new operation and token.
    replies.push(json(409, { error: "conflict" }));
    createLink();
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The request changed while it was being retried. Try again.",
    );
    expect(calls[3].body).toEqual(calls[1].body);

    replies.push(json(201, invitation(firstId)), list(firstId));
    createLink();
    expect(await screen.findByLabelText("Invitation link")).toHaveValue(
      `${window.location.origin}/editor/#${token(2)}`,
    );
    expect(calls[4].body).toEqual({
      operation_id: "operation-id-0002",
      token: token(2),
    });
    await waitFor(() => expect(calls).toHaveLength(6));

    replies.push(json(201, invitation(secondId)), list(firstId, secondId));
    createLink();
    expect(await screen.findByLabelText("Invitation link")).toHaveValue(
      `${window.location.origin}/editor/#${token(3)}`,
    );
    expect(calls[6].body).toEqual({
      operation_id: "operation-id-0003",
      token: token(3),
    });
  });

  it("revokes an invitation and reloads the list", async () => {
    replies.push(list(firstId, secondId));
    renderPanel();
    const rows = await screen.findAllByRole("listitem");

    replies.push(
      json(200, { invitation_id: firstId, status: "revoked" }),
      list(secondId),
    );
    fireEvent.click(within(rows[0]).getByRole("button", { name: "Revoke" }));

    await waitFor(() =>
      expect(screen.getAllByRole("listitem")).toHaveLength(1),
    );
    expect(calls[1]).toEqual({
      method: "POST",
      path: `/api/invitations/${firstId}/revoke`,
      headers: {
        "X-CSRF-Token": "csrf-token-value",
        "Content-Type": "application/json",
      },
      body: { operation_id: "operation-id-0001" },
    });
    expect(calls[2]).toMatchObject({ method: "GET", path: "/api/invitations" });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("says when a revoked invitation was already accepted", async () => {
    replies.push(list(firstId));
    renderPanel();
    const [row] = await screen.findAllByRole("listitem");

    replies.push(
      json(200, { invitation_id: firstId, status: "accepted" }),
      list(),
    );
    fireEvent.click(within(row).getByRole("button", { name: "Revoke" }));

    expect(await screen.findByRole("status")).toHaveTextContent(
      "This invitation was already accepted.",
    );
    expect(
      await screen.findByText("No pending invitations."),
    ).toBeInTheDocument();
  });

  it("retries a revoke with the same operation until it gets an answer", async () => {
    replies.push(list(firstId, secondId));
    renderPanel();
    const rows = await screen.findAllByRole("listitem");

    replies.push("network-error");
    fireEvent.click(within(rows[0]).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The service is unavailable. Try again.",
    );
    replies.push(json(404, { error: "not_found" }));
    fireEvent.click(within(rows[0]).getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "This invitation no longer exists. Reload the page and try again.",
    );
    expect(calls[1].body?.operation_id).toBe("operation-id-0001");
    expect(calls[2].body?.operation_id).toBe("operation-id-0001");

    // After a definitive answer, a new attempt is a new operation.
    replies.push(
      json(200, { invitation_id: firstId, status: "revoked" }),
      list(secondId),
    );
    fireEvent.click(within(rows[0]).getByRole("button", { name: "Revoke" }));
    await waitFor(() => expect(calls).toHaveLength(5));
    expect(calls[3].body?.operation_id).toBe("operation-id-0002");
  });

  it.each([
    [
      "forbidden",
      403,
      "Only the canvas owner can invite moderators. Reload the page and try again.",
    ],
    ["rate_limited", 429, "Too many invitations. Try again later."],
    [
      "conflict",
      409,
      "The request changed while it was being retried. Try again.",
    ],
    ["something_new", 500, "The service is unavailable. Try again."],
  ])(
    "maps a %s creation answer to a fixed message",
    async (code, status, message) => {
      replies.push(list());
      renderPanel();
      await screen.findByText("No pending invitations.");

      replies.push(
        json(status, {
          error: code,
          detail: `token ${token(1)} of invitation ${firstId}`,
        }),
      );
      createLink();

      expect(await screen.findByRole("alert")).toHaveTextContent(message);
      expect(document.body.innerHTML).not.toContain("pci_");
      expect(document.body.textContent).not.toContain(firstId);
      expect(linkField()).not.toBeInTheDocument();
    },
  );

  it("shows a fixed message when the list is unavailable", async () => {
    replies.push(json(500, { error: "internal", detail: firstId }));
    renderPanel();

    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The service is unavailable. Try again.",
    );
    expect(document.body.textContent).not.toContain(firstId);
  });
});
