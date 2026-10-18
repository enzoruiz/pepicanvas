import { afterEach, describe, expect, it, vi } from "vitest";

import { installRestoreGuard } from "./restoreGuard";

function pageTransition(type: "pagehide" | "pageshow", persisted: boolean) {
  const event = new Event(type);
  Object.defineProperty(event, "persisted", { value: persisted });
  return event;
}

describe("installRestoreGuard", () => {
  afterEach(() => {
    document.documentElement.hidden = false;
  });

  it("hides the document before it enters the back/forward cache", () => {
    const target = new EventTarget();
    installRestoreGuard({ target, document, reload: vi.fn() });

    target.dispatchEvent(pageTransition("pagehide", true));

    expect(document.documentElement.hidden).toBe(true);
  });

  it("reloads a page restored from the back/forward cache", () => {
    const target = new EventTarget();
    const reload = vi.fn();
    installRestoreGuard({ target, document, reload });

    target.dispatchEvent(pageTransition("pageshow", true));

    expect(reload).toHaveBeenCalledOnce();
  });

  it("leaves ordinary loads and unloads untouched", () => {
    const target = new EventTarget();
    const reload = vi.fn();
    installRestoreGuard({ target, document, reload });

    target.dispatchEvent(pageTransition("pageshow", false));
    target.dispatchEvent(pageTransition("pagehide", false));

    expect(reload).not.toHaveBeenCalled();
    expect(document.documentElement.hidden).toBe(false);
  });
});
