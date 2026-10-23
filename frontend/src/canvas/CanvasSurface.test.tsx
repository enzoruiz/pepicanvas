import { cleanup, render } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";

import { type CanvasObject, CanvasSurface } from "./CanvasSurface";

afterEach(() => {
  cleanup();
});

function object(change: Partial<CanvasObject>): CanvasObject {
  return {
    id: "obj",
    kind: "text",
    content: "",
    x: 0,
    y: 0,
    width: 100,
    height: 50,
    rotation: 0,
    z: 0,
    ...change,
  };
}

function surfaceOf(container: HTMLElement): HTMLElement {
  const surface = container.querySelector<HTMLElement>("[data-canvas-surface]");
  if (surface === null) throw new Error("no canvas surface");
  return surface;
}

function objectsOf(container: HTMLElement): HTMLElement[] {
  return Array.from(
    surfaceOf(container).querySelectorAll<HTMLElement>("[data-kind]"),
  );
}

describe("CanvasSurface", () => {
  it.each([
    [1920, 1080],
    [320, 180],
    [3840, 2160],
  ])("is exactly %i × %i CSS pixels and clips", (width, height) => {
    const { container } = render(
      <CanvasSurface width={width} height={height} objects={[]} />,
    );

    const surface = surfaceOf(container);
    expect(surface.style.width).toBe(`${width}px`);
    expect(surface.style.height).toBe(`${height}px`);
    expect(surface.style.overflow).toBe("hidden");
    expect(surface.style.position).toBe("relative");
    expect(objectsOf(container)).toHaveLength(0);
  });

  it("places each object with absolute inline styles", () => {
    const { container } = render(
      <CanvasSurface
        width={1920}
        height={1080}
        objects={[
          object({
            id: "secret-object-id",
            content: "Hello stream",
            x: 40,
            y: -15,
            width: 300,
            height: 60,
            rotation: 12.5,
            z: 4,
          }),
          object({
            id: "img",
            kind: "image",
            content: "asset-key",
            x: 5,
            y: 6,
            z: 1,
          }),
        ]}
      />,
    );

    // Sorted by z: the image (z 1) is drawn below the text (z 4).
    const [image, text] = objectsOf(container);
    expect(text).toHaveAttribute("data-kind", "text");
    expect(text).toHaveTextContent("Hello stream");
    expect(text.style.position).toBe("absolute");
    expect(text.style.left).toBe("40px");
    expect(text.style.top).toBe("-15px");
    expect(text.style.width).toBe("300px");
    expect(text.style.height).toBe("60px");
    expect(text.style.transform).toBe("rotate(12.5deg)");
    expect(text.style.zIndex).toBe("4");

    expect(image).toHaveAttribute("data-kind", "image");
    expect(image).toBeEmptyDOMElement();
    expect(image.style.left).toBe("5px");
    expect(image.style.top).toBe("6px");
    expect(image.style.zIndex).toBe("1");

    expect(container.innerHTML).not.toContain("secret-object-id");
    expect(container.innerHTML).not.toContain("asset-key");
  });

  it("keeps objects partly and fully outside the area in the DOM", () => {
    const { container } = render(
      <CanvasSurface
        width={320}
        height={180}
        objects={[
          object({ id: "in", content: "inside", x: 10, y: 10 }),
          object({ id: "part", content: "partial", x: 300, y: 170 }),
          object({ id: "out", content: "outside", x: 1000, y: 900 }),
        ]}
      />,
    );

    const placed = objectsOf(container);
    expect(placed.map((element) => element.textContent)).toEqual([
      "inside",
      "partial",
      "outside",
    ]);
    expect(placed[2].style.left).toBe("1000px");
    expect(placed[2].style.top).toBe("900px");
    expect(surfaceOf(container).style.overflow).toBe("hidden");
  });

  it("stacks objects by z", () => {
    const { container } = render(
      <CanvasSurface
        width={1920}
        height={1080}
        objects={[
          object({ id: "a", content: "top", z: 9 }),
          object({ id: "b", content: "bottom", z: -2 }),
          object({ id: "c", content: "middle", z: 3 }),
        ]}
      />,
    );

    const placed = objectsOf(container);
    expect(
      placed.map((element) => [element.textContent, element.style.zIndex]),
    ).toEqual([
      ["bottom", "-2"],
      ["middle", "3"],
      ["top", "9"],
    ]);
  });
});
