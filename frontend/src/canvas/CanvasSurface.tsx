// The DOM canvas shared by the editor and the OBS overlay (US-007). It
// imports no editor or overlay code and no stylesheet, so the overlay can
// load it without the editor UI. Objects keep their absolute position and
// size; those outside the area stay in the DOM and are clipped.

export interface CanvasObject {
  id: string;
  kind: string;
  content: string;
  x: number;
  y: number;
  width: number;
  height: number;
  rotation: number;
  z: number;
}

interface CanvasSurfaceProps {
  width: number;
  height: number;
  objects: readonly CanvasObject[];
}

export function CanvasSurface({ width, height, objects }: CanvasSurfaceProps) {
  // A stable sort keeps the stored order among objects with the same z.
  const stacked = [...objects].sort((a, b) => a.z - b.z);
  return (
    <div
      data-canvas-surface=""
      style={{ position: "relative", width, height, overflow: "hidden" }}
    >
      {stacked.map((object) => (
        <div
          key={object.id}
          data-kind={object.kind}
          style={{
            position: "absolute",
            left: object.x,
            top: object.y,
            width: object.width,
            height: object.height,
            transform: `rotate(${object.rotation}deg)`,
            zIndex: object.z,
          }}
        >
          {object.kind === "text" ? object.content : null}
        </div>
      ))}
    </div>
  );
}
