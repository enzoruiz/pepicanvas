import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { EditorShell } from "./EditorShell";

describe("EditorShell", () => {
  it("renders the editor mount boundary", () => {
    render(<EditorShell />);

    expect(
      screen.getByRole("main", { name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
  });
});
