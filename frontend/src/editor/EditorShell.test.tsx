import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { EditorShell } from "./EditorShell";

beforeEach(() => {
  vi.stubGlobal(
    "fetch",
    vi.fn(() =>
      Promise.resolve(
        new Response(JSON.stringify({ error: "unauthorized" }), {
          status: 401,
        }),
      ),
    ),
  );
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

describe("EditorShell", () => {
  it("renders the editor mount boundary", async () => {
    render(<EditorShell />);

    expect(
      screen.getByRole("main", { name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("heading", { level: 1, name: "PepiCanvas editor" }),
    ).toBeInTheDocument();
    expect(
      await screen.findByRole("heading", { level: 2, name: "Sign in" }),
    ).toBeInTheDocument();
  });
});
