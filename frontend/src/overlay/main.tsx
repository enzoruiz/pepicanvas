import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { OverlayShell } from "./OverlayShell";

const overlayRoot = document.querySelector("#overlay-root");

if (!(overlayRoot instanceof HTMLElement)) {
  throw new Error("Overlay root element not found");
}

createRoot(overlayRoot).render(
  <StrictMode>
    <OverlayShell />
  </StrictMode>,
);
