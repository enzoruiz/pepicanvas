import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { OperatorPanel } from "./OperatorPanel";

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
  email: "olga@example.com",
  csrfToken: "csrf-token-value",
  operator: true,
};

function activation(created: boolean): Reply {
  return json(200, {
    account_id: "11111111111111111111111111111111",
    tenant_id: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
    canvas_id: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    created,
  });
}

function renderPanel() {
  let next = 0;
  return render(
    <OperatorPanel
      session={session}
      newOperationId={() => `operation-id-${String(++next).padStart(4, "0")}`}
    />,
  );
}

function activateEmail(email: string) {
  fireEvent.change(screen.getByLabelText("Streamer email"), {
    target: { value: email },
  });
  fireEvent.click(screen.getByRole("button", { name: "Activate" }));
}

describe("OperatorPanel", () => {
  it("activates a streamer with the CSRF token and clears the email", async () => {
    renderPanel();
    expect(
      screen.getByRole("region", { name: "Activate a streamer" }),
    ).toBeInTheDocument();

    replies.push(activation(true));
    activateEmail("bob@example.com");

    expect(await screen.findByRole("status")).toHaveTextContent(
      "Streamer activated.",
    );
    expect(calls[0]).toMatchObject({
      method: "POST",
      path: "/api/activations",
      headers: { "X-CSRF-Token": "csrf-token-value" },
      body: { operation_id: "operation-id-0001", email: "bob@example.com" },
    });
    expect(screen.getByLabelText("Streamer email")).toHaveValue("");
    expect(document.body.textContent).not.toContain("aaaaaaaa");
    expect(document.body.textContent).not.toContain("bbbbbbbb");
  });

  it("says a repeated activation left the canvas unchanged", async () => {
    renderPanel();

    replies.push(activation(false));
    activateEmail("bob@example.com");

    expect(await screen.findByRole("status")).toHaveTextContent(
      "Streamer was already active; the canvas is unchanged.",
    );
  });

  it("retries with the same operation ID until it gets an answer", async () => {
    renderPanel();

    replies.push("network-error");
    activateEmail("bob@example.com");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The service is unavailable. Try again.",
    );
    expect(screen.getByLabelText("Streamer email")).toHaveValue(
      "bob@example.com",
    );
    replies.push(json(503, { error: "unavailable" }));
    fireEvent.click(screen.getByRole("button", { name: "Activate" }));
    await waitFor(() => expect(calls).toHaveLength(2));
    expect(calls[0].body?.operation_id).toBe("operation-id-0001");
    expect(calls[1].body?.operation_id).toBe("operation-id-0001");

    // A changed email is a new intent with a new operation ID.
    replies.push(json(404, { error: "not_found" }));
    activateEmail("carol@example.com");
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "No account uses this email.",
    );
    expect(calls[2].body?.operation_id).toBe("operation-id-0002");

    // After a definitive answer, even the same email starts a new operation.
    replies.push(activation(true));
    fireEvent.click(screen.getByRole("button", { name: "Activate" }));
    expect(await screen.findByRole("status")).toHaveTextContent(
      "Streamer activated.",
    );
    expect(calls[3].body?.operation_id).toBe("operation-id-0003");
    expect(calls[3].body?.email).toBe("carol@example.com");

    replies.push(activation(false));
    activateEmail("carol@example.com");
    await screen.findByText(
      "Streamer was already active; the canvas is unchanged.",
    );
    expect(calls[4].body?.operation_id).toBe("operation-id-0004");
  });

  it.each([
    ["bad_request", 400, "Enter a valid email address."],
    [
      "forbidden",
      403,
      "Only operators can activate streamers. Reload the page and try again.",
    ],
    [
      "conflict",
      409,
      "The request changed while it was being retried. Try again.",
    ],
    ["not_found", 404, "No account uses this email."],
    ["something_new", 500, "The service is unavailable. Try again."],
  ])("maps %s to a fixed message", async (code, status, message) => {
    renderPanel();

    replies.push(
      json(status, {
        error: code,
        detail: "account 2222 of mallory@example.com",
      }),
    );
    activateEmail("bob@example.com");

    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(document.body.textContent).not.toContain("mallory@example.com");
    expect(document.body.textContent).not.toContain("2222");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});
