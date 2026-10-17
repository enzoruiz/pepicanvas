import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { ClientSupportGuard } from "./ClientSupportGuard";
import type { ClientSnapshot } from "./clientSupport";

const chromeUserAgent =
  "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
  "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36";

function supportedSnapshot(): ClientSnapshot {
  return {
    userAgent: chromeUserAgent,
    userAgentDataMobile: false,
    maxTouchPoints: 0,
    obsStudioIntegration: false,
    viewportWidth: 1280,
    viewportHeight: 720,
  };
}

function StatefulChild() {
  const [count, setCount] = useState(0);

  return (
    <button type="button" onClick={() => setCount((value) => value + 1)}>
      Count: {count}
    </button>
  );
}

describe("ClientSupportGuard", () => {
  it("blocks accessibly, preserves child state, and resumes after resize", () => {
    let currentSnapshot = supportedSnapshot();

    render(
      <ClientSupportGuard readSnapshot={() => currentSnapshot}>
        <StatefulChild />
      </ClientSupportGuard>,
    );

    fireEvent.click(screen.getByRole("button", { name: "Count: 0" }));
    expect(screen.getByRole("button", { name: "Count: 1" })).toBeEnabled();

    currentSnapshot = { ...currentSnapshot, viewportWidth: 1279 };
    fireEvent.resize(window);

    expect(screen.getByRole("alert")).toHaveAccessibleName(
      "Unsupported client",
    );
    expect(screen.getByText(/1280 by 720/)).toBeInTheDocument();
    expect(screen.getByText(/OBS Browser Source/)).toBeInTheDocument();

    const guardedContent = screen.getByTestId("client-support-content");
    expect(guardedContent).toHaveAttribute("inert");
    expect(guardedContent).toHaveAttribute("aria-hidden", "true");
    expect(guardedContent).toHaveStyle({ pointerEvents: "none" });

    fireEvent.click(screen.getByRole("button", { hidden: true }));
    expect(screen.getByRole("button", { hidden: true })).toHaveTextContent(
      "Count: 1",
    );

    currentSnapshot = { ...currentSnapshot, viewportWidth: 1280 };
    fireEvent.resize(window);

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Count: 1" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "Count: 1" }));
    expect(screen.getByRole("button", { name: "Count: 2" })).toBeEnabled();
  });

  it("removes the same resize listener during effect cleanup", () => {
    const addEventListener = vi.spyOn(window, "addEventListener");
    const removeEventListener = vi.spyOn(window, "removeEventListener");

    const { unmount } = render(
      <ClientSupportGuard readSnapshot={supportedSnapshot}>
        <div>Application</div>
      </ClientSupportGuard>,
    );

    const resizeRegistration = addEventListener.mock.calls.find(
      ([eventName]) => eventName === "resize",
    );
    expect(resizeRegistration).toBeDefined();

    unmount();

    expect(removeEventListener).toHaveBeenCalledWith(
      "resize",
      resizeRegistration?.[1],
    );
  });
});
