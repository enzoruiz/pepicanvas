import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AuthGate } from "./AuthGate";

const password = "decoy-password-5d1e7b";

interface Call {
  method: string;
  path: string;
  headers: Record<string, string>;
  body: Record<string, unknown> | undefined;
}

type Reply = { status: number; body?: unknown } | "network-error";

let calls: Call[];
let replies: Reply[];

function json(status: number, body?: unknown): Reply {
  return { status, body };
}

beforeEach(() => {
  calls = [];
  replies = [];
  vi.stubGlobal(
    "fetch",
    vi.fn((path: string, init: RequestInit) => {
      calls.push({
        method: init.method ?? "GET",
        path,
        headers: init.headers as Record<string, string>,
        body:
          typeof init.body === "string"
            ? (JSON.parse(init.body) as Record<string, unknown>)
            : undefined,
      });
      const reply = replies.shift();
      if (reply === undefined) throw new Error(`unexpected ${path}`);
      if (reply === "network-error") {
        return Promise.reject(new TypeError("Failed to fetch"));
      }
      return Promise.resolve(
        new Response(
          reply.body === undefined ? null : JSON.stringify(reply.body),
          { status: reply.status },
        ),
      );
    }),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const session = {
  account_id: "0123456789abcdef0123456789abcdef",
  email: "alice@example.com",
  csrf_token: "csrf-token-value",
  operator: false,
};

function renderGate(newOperationId?: () => string) {
  return render(
    <AuthGate newOperationId={newOperationId}>
      {(s) => <p>editor for {s.accountId}</p>}
    </AuthGate>,
  );
}

function fill(email: string, pass: string) {
  fireEvent.change(screen.getByLabelText("Email"), {
    target: { value: email },
  });
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: pass },
  });
}

function click(name: string) {
  fireEvent.click(screen.getByRole("button", { name }));
}

describe("AuthGate", () => {
  it("asks the server for the session and shows sign-in when there is none", async () => {
    replies.push(json(401, { error: "unauthorized", correlation_id: "c" }));
    renderGate();

    expect(
      await screen.findByRole("heading", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(calls[0]).toMatchObject({ method: "GET", path: "/api/session" });
    expect(screen.queryByText(/editor for/)).not.toBeInTheDocument();
  });

  it("shows a note next to sign-in and hides it once signed in", async () => {
    const note = "Sign in to continue.";
    replies.push(json(401, { error: "unauthorized" }));
    render(
      <AuthGate note={note}>{(s) => <p>editor for {s.accountId}</p>}</AuthGate>,
    );

    expect(await screen.findByText(note)).toBeInTheDocument();
    click("Create an account");
    expect(screen.getByText(note)).toBeInTheDocument();
    click("I already have an account");

    replies.push(json(201, session));
    fill("alice@example.com", password);
    click("Sign in");
    expect(
      await screen.findByText(`editor for ${session.account_id}`),
    ).toBeInTheDocument();
    expect(screen.queryByText(note)).not.toBeInTheDocument();
  });

  it("restores a valid session from the server", async () => {
    replies.push(json(200, session));
    renderGate();

    expect(
      await screen.findByText(`editor for ${session.account_id}`),
    ).toBeInTheDocument();
    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
  });

  it("signs in and shows the editor", async () => {
    replies.push(json(401, { error: "unauthorized" }));
    renderGate();
    await screen.findByRole("heading", { name: "Sign in" });

    replies.push(json(201, session));
    fill("alice@example.com", password);
    click("Sign in");

    expect(
      await screen.findByText(`editor for ${session.account_id}`),
    ).toBeInTheDocument();
    expect(calls[1]).toMatchObject({
      method: "POST",
      path: "/api/sessions",
      body: { email: "alice@example.com", password },
    });
  });

  it("shows only a safe message for invalid credentials", async () => {
    replies.push(json(401, { error: "unauthorized" }));
    renderGate();
    await screen.findByRole("heading", { name: "Sign in" });

    replies.push(
      json(401, {
        error: "unauthorized",
        detail: "pcs_leakedtoken for bob@example.com",
      }),
    );
    fill("alice@example.com", password);
    click("Sign in");

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Email or password is incorrect.");
    expect(document.body.textContent).not.toContain(password);
    expect(document.body.textContent).not.toContain("pcs_");
    expect(document.body.textContent).not.toContain("bob@example.com");
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(document.body.innerHTML).not.toContain(password);
    expect(screen.queryByText(/editor for/)).not.toBeInTheDocument();
  });

  it.each([
    ["rate_limited", "Too many attempts. Wait a few minutes and try again."],
    ["forbidden", "The request was rejected. Reload the page and try again."],
    ["something_new", "The service is unavailable. Try again."],
  ])("maps %s to a fixed message", async (code, message) => {
    replies.push(json(401, { error: "unauthorized" }));
    renderGate();
    await screen.findByRole("heading", { name: "Sign in" });

    replies.push(json(429, { error: code }));
    fill("alice@example.com", password);
    click("Sign in");

    expect(await screen.findByRole("alert")).toHaveTextContent(message);
  });

  it("registers, then asks the person to sign in", async () => {
    replies.push(json(401, { error: "unauthorized" }));
    renderGate(() => "op-11111111-1111-1111-1111-111111111111");
    fireEvent.click(
      await screen.findByRole("button", { name: "Create an account" }),
    );

    replies.push(json(201, { account_id: session.account_id }));
    fill("alice@example.com", password);
    click("Create account");

    expect(
      await screen.findByText("Account created. You can sign in now."),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(calls[1]).toMatchObject({
      method: "POST",
      path: "/api/accounts",
      body: {
        operation_id: "op-11111111-1111-1111-1111-111111111111",
        email: "alice@example.com",
        password,
      },
    });
  });

  it("retries a registration with the same operation ID until it gets an answer", async () => {
    let next = 0;
    replies.push(json(401, { error: "unauthorized" }));
    renderGate(() => `operation-id-${String(++next).padStart(4, "0")}`);
    fireEvent.click(
      await screen.findByRole("button", { name: "Create an account" }),
    );
    fill("alice@example.com", password);

    replies.push("network-error");
    click("Create account");
    await screen.findByRole("alert");
    replies.push("network-error");
    click("Create account");
    await waitFor(() => expect(calls).toHaveLength(3));
    expect(calls[1].body?.operation_id).toBe("operation-id-0001");
    expect(calls[2].body?.operation_id).toBe("operation-id-0001");

    // A changed payload is a new intent with a new operation ID.
    replies.push(json(409, { error: "identity_taken" }));
    fill("bob@example.com", password);
    click("Create account");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "An account with this email already exists.",
    );
    expect(calls[3].body?.operation_id).toBe("operation-id-0002");

    // After a definitive answer, even the same payload starts a new operation.
    replies.push(json(409, { error: "identity_taken" }));
    click("Create account");
    await waitFor(() => expect(calls).toHaveLength(5));
    expect(calls[4].body?.operation_id).toBe("operation-id-0003");
  });

  it("signs out with the session CSRF token", async () => {
    replies.push(json(200, session));
    renderGate();

    replies.push(json(204));
    fireEvent.click(await screen.findByRole("button", { name: "Sign out" }));

    expect(
      await screen.findByRole("heading", { name: "Sign in" }),
    ).toBeInTheDocument();
    expect(calls[1]).toMatchObject({
      method: "DELETE",
      path: "/api/session",
      headers: { "X-CSRF-Token": "csrf-token-value" },
    });
  });
});
