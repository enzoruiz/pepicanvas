import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { ClientSupportGuard } from "../client-support/ClientSupportGuard";
import { installWindowRestoreGuard } from "../privacy/restoreGuard";
import { OverlayShell } from "./OverlayShell";

installWindowRestoreGuard();

const overlayRoot = document.querySelector("#overlay-root");

if (!(overlayRoot instanceof HTMLElement)) {
  throw new Error("Overlay root element not found");
}

createRoot(overlayRoot).render(
  <StrictMode>
    <ClientSupportGuard>
      <OverlayShell />
    </ClientSupportGuard>
  </StrictMode>,
);
