import { act, cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { type Call, json, type Reply, stubFetch } from "../test/fetchStub";
import { LockOwners } from "./LockOwners";

let calls: Call[];
let replies: Reply[];

beforeEach(() => {
  vi.useFakeTimers();
  ({ calls, replies } = stubFetch());
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

function locks(...entries: [string, string, boolean][]): Reply {
  return json(200, {
    locks: entries.map(([object_id, name, mine]) => ({
      object_id,
      holder: { name, mine },
    })),
  });
}

async function settle() {
  await act(() => vi.advanceTimersByTimeAsync(0));
}

describe("LockOwners", () => {
  it("names each holder, and the caller as you", async () => {
    replies.push(
      locks(["obj-1", "bob@example.com", false], ["obj-2", "alice@x.io", true]),
    );
    render(<LockOwners tenantId="tenant-a" canvasId="canvas-1" />);
    await settle();
    expect(calls.map((call) => [call.method, call.path])).toEqual([
      ["GET", "/api/tenants/tenant-a/canvases/canvas-1/locks"],
    ]);
    const list = screen.getByRole("list", { name: "Locked objects" });
    expect(
      [...list.querySelectorAll("li")].map((item) => [
        item.dataset.objectId,
        item.textContent,
      ]),
    ).toEqual([
      ["obj-1", "Locked by bob@example.com"],
      ["obj-2", "Locked by you"],
    ]);
  });

  it("follows the holders the server reports on every poll", async () => {
    replies.push(locks(["obj-1", "bob@example.com", false]));
    render(
      <LockOwners tenantId="tenant-a" canvasId="canvas-1" pollMs={2000} />,
    );
    await settle();
    expect(screen.getByText("Locked by bob@example.com")).toBeInTheDocument();

    replies.push(locks(["obj-1", "carol@example.com", false]));
    await act(() => vi.advanceTimersByTimeAsync(1999));
    expect(calls).toHaveLength(1);
    await act(() => vi.advanceTimersByTimeAsync(1));
    expect(calls).toHaveLength(2);
    expect(screen.getByText("Locked by carol@example.com")).toBeInTheDocument();

    replies.push(locks());
    await act(() => vi.advanceTimersByTimeAsync(2000));
    expect(screen.queryByRole("list")).not.toBeInTheDocument();
  });

  it("shows only the holder's name", async () => {
    replies.push(
      json(200, {
        locks: [
          {
            object_id: "obj-1",
            holder: { name: "", mine: false, account_id: "decoy-account" },
            lock_session_id: "pls_decoy",
          },
        ],
      }),
    );
    const { container } = render(
      <LockOwners tenantId="tenant-a" canvasId="canvas-1" />,
    );
    await settle();
    expect(screen.getByText("Locked by another participant")).toBeVisible();
    expect(container.innerHTML).not.toContain("decoy");
  });

  it("shows nothing when the holders are unavailable", async () => {
    replies.push(json(503, { error: "lock_unavailable" }));
    const { container } = render(
      <LockOwners tenantId="tenant-a" canvasId="canvas-1" />,
    );
    await settle();
    expect(container).toBeEmptyDOMElement();
  });

  it("stops polling once unmounted", async () => {
    replies.push(locks());
    const { unmount } = render(
      <LockOwners tenantId="tenant-a" canvasId="canvas-1" pollMs={2000} />,
    );
    await settle();
    unmount();
    await act(() => vi.advanceTimersByTimeAsync(10_000));
    expect(calls).toHaveLength(1);
  });
});
