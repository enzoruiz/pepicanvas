import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { OverlayShell } from "./OverlayShell";

describe("OverlayShell", () => {
  it("renders the overlay mount boundary", () => {
    render(<OverlayShell />);

    expect(
      screen.getByRole("main", { name: "PepiCanvas overlay" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: "PepiCanvas overlay" }),
    ).toBeInTheDocument();
  });
});
