import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { AcceptInvitationPanel } from "./AcceptInvitationPanel";
import { acceptOperationKey } from "./invitationLink";

let calls: Call[];
let replies: Reply[];

const token = `pci_${"k".repeat(43)}`;
const tenantId = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa";
const canvasId = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb";

const session = {
  accountId: "0123456789abcdef0123456789abcdef",
  email: "bob@example.com",
  csrfToken: "csrf-token-value",
  operator: false,
};

const intro = "You were invited to moderate a streamer's canvas.";
const accepted = "You are now a moderator of this canvas.";
const unusable =
  "This invitation link can't be used. Ask the streamer for a new one.";
const conflict = "The request changed while it was being retried. Try again.";
const unavailable = "The service is unavailable. Try again.";
const rateLimited = "Too many attempts. Wait a few minutes and try again.";

let nextOperation: number;
let newOperationId: ReturnType<typeof vi.fn<() => string>>;
let onSettled: ReturnType<typeof vi.fn<(accepted: boolean) => void>>;

beforeEach(() => {
  ({ calls, replies } = stubFetch());
  window.sessionStorage.clear();
  window.history.replaceState(null, "", `/editor/#${token}`);
  nextOperation = 0;
  newOperationId = vi.fn(
    () => `operation-id-${String(++nextOperation).padStart(4, "0")}`,
  );
  onSettled = vi.fn<(accepted: boolean) => void>();
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
  window.sessionStorage.clear();
  window.history.replaceState(null, "", "/");
});

function renderPanel(current: string | null = token) {
  return render(
    <AcceptInvitationPanel
      session={session}
      token={current}
      newOperationId={newOperationId}
      onSettled={onSettled}
    />,
  );
}

function accept() {
  fireEvent.click(screen.getByRole("button", { name: "Accept invitation" }));
}

function acceptCall(operationId: string): Call {
  return {
    method: "POST",
    path: "/api/invitations/accept",
    headers: {
      "X-CSRF-Token": "csrf-token-value",
      "Content-Type": "application/json",
    },
    body: { operation_id: operationId, token },
  };
}

async function storedOperation() {
  return window.sessionStorage.getItem(await acceptOperationKey(token));
}

function storageHoldsNoToken() {
  for (let i = 0; i < window.sessionStorage.length; i++) {
    const key = window.sessionStorage.key(i) ?? "";
    expect(key).not.toContain("pci_");
    expect(window.sessionStorage.getItem(key)).not.toContain("pci_");
  }
}

describe("AcceptInvitationPanel", () => {
  it("asks for an explicit confirmation and sends nothing before it", async () => {
    renderPanel();

    expect(screen.getByText(intro)).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeEnabled();
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls).toHaveLength(0);
    expect(window.sessionStorage).toHaveLength(0);
    expect(document.body.textContent).not.toContain("pci_");
  });

  it("renders nothing without a token", () => {
    const { container } = renderPanel(null);

    expect(container).toBeEmptyDOMElement();
  });

  it("accepts, clears the fragment and the stored operation, and reports it", async () => {
    const replaceState = vi.spyOn(window.history, "replaceState");
    renderPanel();

    replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));
    accept();

    expect(await screen.findByText(accepted)).toBeInTheDocument();
    expect(calls).toEqual([acceptCall("operation-id-0001")]);
    expect(replaceState).toHaveBeenCalledWith(null, "", "/editor/");
    expect(window.location.hash).toBe("");
    expect(window.sessionStorage).toHaveLength(0);
    expect(onSettled).toHaveBeenCalledExactlyOnceWith(true);
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain(tenantId);
    expect(document.body.textContent).not.toContain(canvasId);
  });

  it.each([
    ["a network failure", "network-error" as const],
    ["an internal error", json(500, { error: "internal" })],
    ["an unavailable server", json(503, { error: "unavailable" })],
  ])(
    "keeps the operation and the fragment after %s and retries with the same ID",
    async (_, failure) => {
      renderPanel();

      replies.push(failure);
      accept();

      expect(await screen.findByText(unavailable)).toBeInTheDocument();
      expect(await storedOperation()).toBe("operation-id-0001");
      storageHoldsNoToken();
      expect(window.location.hash).toBe(`#${token}`);
      expect(onSettled).not.toHaveBeenCalled();

      replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));
      accept();

      expect(await screen.findByText(accepted)).toBeInTheDocument();
      expect(calls).toEqual([
        acceptCall("operation-id-0001"),
        acceptCall("operation-id-0001"),
      ]);
      expect(newOperationId).toHaveBeenCalledTimes(1);
      expect(window.sessionStorage).toHaveLength(0);
    },
  );

  it("reuses the stored operation ID after a remount, as after a reload", async () => {
    const first = renderPanel();
    replies.push("network-error");
    accept();
    expect(await screen.findByText(unavailable)).toBeInTheDocument();
    first.unmount();

    renderPanel();
    expect(screen.queryByText(unavailable)).not.toBeInTheDocument();
    replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));
    accept();

    expect(await screen.findByText(accepted)).toBeInTheDocument();
    expect(calls).toEqual([
      acceptCall("operation-id-0001"),
      acceptCall("operation-id-0001"),
    ]);
    expect(newOperationId).toHaveBeenCalledTimes(1);
  });

  it("clears the fragment and the operation for an unusable link", async () => {
    renderPanel();

    replies.push(json(404, { error: "not_found" }));
    accept();

    expect(await screen.findByText(unusable)).toBeInTheDocument();
    expect(window.location.hash).toBe("");
    expect(window.sessionStorage).toHaveLength(0);
    expect(onSettled).toHaveBeenCalledExactlyOnceWith(false);
    expect(
      screen.queryByRole("button", { name: "Accept invitation" }),
    ).not.toBeInTheDocument();
  });

  it("renews the operation ID after a conflict", async () => {
    renderPanel();

    replies.push(json(409, { error: "conflict" }));
    accept();

    expect(await screen.findByText(conflict)).toBeInTheDocument();
    expect(window.sessionStorage).toHaveLength(0);
    expect(window.location.hash).toBe(`#${token}`);
    expect(onSettled).not.toHaveBeenCalled();

    replies.push(json(200, { tenant_id: tenantId, canvas_id: canvasId }));
    accept();

    expect(await screen.findByText(accepted)).toBeInTheDocument();
    expect(calls).toEqual([
      acceptCall("operation-id-0001"),
      acceptCall("operation-id-0002"),
    ]);
  });

  it("keeps the operation when rate limited", async () => {
    renderPanel();

    replies.push(json(429, { error: "rate_limited" }));
    accept();

    expect(await screen.findByText(rateLimited)).toBeInTheDocument();
    expect(await storedOperation()).toBe("operation-id-0001");
    expect(window.location.hash).toBe(`#${token}`);
    expect(
      screen.getByRole("button", { name: "Accept invitation" }),
    ).toBeEnabled();
  });

  it("shows only fixed text for an unknown error", async () => {
    renderPanel();

    replies.push(json(400, { error: "bad_request", detail: token }));
    accept();

    await waitFor(() => expect(calls).toHaveLength(1));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The request was not accepted. Reload the page and try again.",
    );
    expect(document.body.textContent).not.toContain("pci_");
    expect(window.sessionStorage).toHaveLength(0);
  });
});
