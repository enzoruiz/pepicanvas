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
import { ModeratorsPanel } from "./ModeratorsPanel";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  ({ calls, replies } = stubFetch());
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

const tenantId = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const bob = {
  account_id: "11111111111111111111111111111111",
  email: "bob@example.com",
  accepted_at: "2026-10-28T10:00:00Z",
};
const carol = {
  account_id: "22222222222222222222222222222222",
  email: "carol@example.com",
  accepted_at: null,
};

const revokedMessage =
  "Moderator access revoked. Their confirmed changes are kept.";
const goneMessage = "That moderator no longer has access.";
const unavailableMessage = "The service is unavailable. Try again.";

function members(...entries: (typeof bob | typeof carol)[]): Reply {
  return json(200, { members: entries });
}

function revoked(accountId: string): Reply {
  return json(200, { tenant_id: tenantId, account_id: accountId });
}

function renderPanel() {
  let next = 0;
  return render(
    <ModeratorsPanel
      session={session}
      newOperationId={() => `operation-id-${String(++next).padStart(4, "0")}`}
    />,
  );
}

function row(email: string) {
  const item = screen
    .getAllByRole("listitem")
    .find((candidate) => candidate.textContent?.includes(email));
  if (!item) throw new Error(`no row for ${email}`);
  return item;
}

function askToRevoke(email: string) {
  fireEvent.click(within(row(email)).getByRole("button", { name: "Revoke" }));
}

function confirmRevoke(email: string) {
  fireEvent.click(
    within(row(email)).getByRole("button", { name: "Confirm revoke" }),
  );
}

describe("ModeratorsPanel", () => {
  it("lists each moderator's email and acceptance date with a revoke button", async () => {
    replies.push(members(bob, carol));
    renderPanel();

    expect(
      screen.getByRole("heading", { level: 2, name: "Moderators" }),
    ).toBeInTheDocument();
    const rows = await screen.findAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(calls).toEqual([
      { method: "GET", path: "/api/members", headers: {}, body: undefined },
    ]);
    expect(rows[0]).toHaveTextContent("bob@example.com");
    expect(rows[0].querySelector("time")).toHaveAttribute(
      "dateTime",
      "2026-10-28T10:00:00.000Z",
    );
    expect(rows[0]).toHaveTextContent(/Accepted /);
    expect(rows[1]).toHaveTextContent("carol@example.com");
    expect(rows[1]).toHaveTextContent("Acceptance date not recorded");
    for (const item of rows) {
      expect(
        within(item).getByRole("button", { name: "Revoke" }),
      ).toBeInTheDocument();
    }
    expect(document.body.textContent).not.toContain(bob.account_id);
  });

  it("says when there are no moderators", async () => {
    replies.push(members());
    renderPanel();

    expect(await screen.findByText("No moderators yet.")).toBeInTheDocument();
    expect(screen.queryByRole("listitem")).not.toBeInTheDocument();
  });

  it("asks for an explicit confirmation before revoking", async () => {
    replies.push(members(bob, carol));
    renderPanel();
    await screen.findAllByRole("listitem");

    askToRevoke("bob@example.com");
    expect(calls).toHaveLength(1);
    expect(
      within(row("bob@example.com")).getByRole("button", {
        name: "Confirm revoke",
      }),
    ).toBeInTheDocument();
    expect(
      within(row("carol@example.com")).queryByRole("button", {
        name: "Confirm revoke",
      }),
    ).not.toBeInTheDocument();

    fireEvent.click(
      within(row("bob@example.com")).getByRole("button", { name: "Cancel" }),
    );
    expect(
      within(row("bob@example.com")).queryByRole("button", {
        name: "Confirm revoke",
      }),
    ).not.toBeInTheDocument();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls).toHaveLength(1);
  });

  it("revokes a confirmed moderator and removes the row", async () => {
    replies.push(members(bob, carol));
    renderPanel();
    await screen.findAllByRole("listitem");

    replies.push(revoked(bob.account_id));
    askToRevoke("bob@example.com");
    confirmRevoke("bob@example.com");

    expect(await screen.findByRole("status")).toHaveTextContent(revokedMessage);
    expect(screen.getAllByRole("listitem")).toHaveLength(1);
    expect(screen.queryByText("bob@example.com")).not.toBeInTheDocument();
    expect(screen.getByText("carol@example.com")).toBeInTheDocument();
    expect(calls.slice(1)).toEqual([
      {
        method: "POST",
        path: `/api/members/${bob.account_id}/revoke`,
        headers: {
          "X-CSRF-Token": "csrf-token-value",
          "Content-Type": "application/json",
        },
        body: { operation_id: "operation-id-0001" },
      },
    ]);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("says when the last moderator is revoked", async () => {
    replies.push(members(bob));
    renderPanel();
    await screen.findAllByRole("listitem");

    replies.push(revoked(bob.account_id));
    askToRevoke("bob@example.com");
    confirmRevoke("bob@example.com");

    expect(await screen.findByText("No moderators yet.")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(revokedMessage);
  });

  it("says a moderator no longer has access and reloads the list", async () => {
    replies.push(members(bob, carol));
    renderPanel();
    await screen.findAllByRole("listitem");

    replies.push(json(404, { error: "not_found" }), members(carol));
    askToRevoke("bob@example.com");
    confirmRevoke("bob@example.com");

    expect(await screen.findByRole("status")).toHaveTextContent(goneMessage);
    await waitFor(() =>
      expect(screen.getAllByRole("listitem")).toHaveLength(1),
    );
    expect(calls.slice(1).map((call) => [call.method, call.path])).toEqual([
      ["POST", `/api/members/${bob.account_id}/revoke`],
      ["GET", "/api/members"],
    ]);
    expect(screen.queryByText("bob@example.com")).not.toBeInTheDocument();
  });

  it("retries a revoke with the same operation until it gets an answer", async () => {
    replies.push(members(bob, carol));
    renderPanel();
    await screen.findAllByRole("listitem");

    replies.push("network-error");
    askToRevoke("bob@example.com");
    confirmRevoke("bob@example.com");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      unavailableMessage,
    );
    expect(screen.getByText("bob@example.com")).toBeInTheDocument();

    replies.push(json(503, { error: "unavailable" }));
    confirmRevoke("bob@example.com");
    await waitFor(() => expect(calls).toHaveLength(3));
    await waitFor(() =>
      expect(
        within(row("bob@example.com")).getByRole("button", {
          name: "Confirm revoke",
        }),
      ).toBeEnabled(),
    );

    // Another account gets its own operation; bob's stays pending.
    replies.push(json(409, { error: "conflict" }));
    askToRevoke("carol@example.com");
    confirmRevoke("carol@example.com");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The request changed while it was being retried. Try again.",
    );

    replies.push(revoked(bob.account_id));
    askToRevoke("bob@example.com");
    confirmRevoke("bob@example.com");
    expect(await screen.findByRole("status")).toHaveTextContent(revokedMessage);

    // After a definitive answer, a new attempt is a new operation.
    replies.push(revoked(carol.account_id));
    askToRevoke("carol@example.com");
    confirmRevoke("carol@example.com");
    await waitFor(() => expect(calls).toHaveLength(6));

    expect(
      calls.slice(1).map((call) => [call.path, call.body?.operation_id]),
    ).toEqual([
      [`/api/members/${bob.account_id}/revoke`, "operation-id-0001"],
      [`/api/members/${bob.account_id}/revoke`, "operation-id-0001"],
      [`/api/members/${carol.account_id}/revoke`, "operation-id-0002"],
      [`/api/members/${bob.account_id}/revoke`, "operation-id-0001"],
      [`/api/members/${carol.account_id}/revoke`, "operation-id-0003"],
    ]);
  });

  it.each([
    [
      "bad_request",
      400,
      "The request was not accepted. Reload the page and try again.",
    ],
    [
      "forbidden",
      403,
      "Only the canvas owner can revoke moderators. Reload the page and try again.",
    ],
    [
      "conflict",
      409,
      "The request changed while it was being retried. Try again.",
    ],
    ["unauthorized", 401, "Your session has ended. Sign in again."],
    ["something_new", 500, unavailableMessage],
  ])(
    "maps a %s revocation answer to a fixed message",
    async (code, status, message) => {
      replies.push(members(bob));
      renderPanel();
      await screen.findAllByRole("listitem");

      replies.push(
        json(status, {
          error: code,
          detail: `account ${bob.account_id} tenant ${tenantId}`,
        }),
      );
      askToRevoke("bob@example.com");
      confirmRevoke("bob@example.com");

      expect(await screen.findByRole("alert")).toHaveTextContent(message);
      expect(document.body.textContent).not.toContain(bob.account_id);
      expect(document.body.textContent).not.toContain(tenantId);
      expect(screen.getByText("bob@example.com")).toBeInTheDocument();
    },
  );

  it.each([
    ["forbidden", 403, "Only the canvas owner can see its moderators."],
    ["unauthorized", 401, "Your session has ended. Sign in again."],
    [
      "internal",
      500,
      "The moderators could not be loaded. Reload the page to try again.",
    ],
  ])(
    "shows a list-specific message when the list fails with %s",
    async (error, status, message) => {
      replies.push(json(status, { error, detail: bob.email }));
      renderPanel();

      expect(await screen.findByRole("alert")).toHaveTextContent(message);
      expect(document.body.textContent).not.toContain("revoke");
      expect(document.body.textContent).not.toContain(bob.email);
    },
  );
});
