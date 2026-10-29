import { useState } from "react";

// The DOM canvas shared by the editor and the OBS overlay (US-007). It
// imports no editor or overlay code and no stylesheet, so the overlay can
// load it without the editor UI. Objects keep their absolute position and
// size; those outside the area stay in the DOM and are clipped. An image
// object is an <img>, so a GIF animates natively (US-011); its URL comes
// from the host, which knows how its canvas serves media.

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
  // The URL of an image object's content, or undefined to leave it empty.
  mediaSource?: (content: string) => string | undefined;
}

// Hidden until it loaded completely, so a box never shows a partial or
// broken image; a failed image leaves the box empty.
function MediaImage({ src }: { src: string }) {
  const [loaded, setLoaded] = useState(false);
  return (
    <img
      src={src}
      alt=""
      draggable={false}
      decoding="async"
      onLoad={() => setLoaded(true)}
      onError={() => setLoaded(false)}
      style={{
        display: "block",
        width: "100%",
        height: "100%",
        objectFit: "contain",
        visibility: loaded ? "visible" : "hidden",
      }}
    />
  );
}

function ObjectContent({
  object,
  mediaSource,
}: {
  object: CanvasObject;
  mediaSource?: (content: string) => string | undefined;
}) {
  if (object.kind === "text") return object.content;
  if (object.kind === "image") {
    const src = mediaSource?.(object.content);
    // A new URL is a new image, which starts hidden again.
    return src === undefined ? null : <MediaImage key={src} src={src} />;
  }
  return null;
}

export function CanvasSurface({
  width,
  height,
  objects,
  mediaSource,
}: CanvasSurfaceProps) {
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
          <ObjectContent object={object} mediaSource={mediaSource} />
        </div>
      ))}
    </div>
  );
}
