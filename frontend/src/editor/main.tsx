import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { ClientSupportGuard } from "../client-support/ClientSupportGuard";
import { installWindowRestoreGuard } from "../privacy/restoreGuard";
import { EditorShell } from "./EditorShell";

installWindowRestoreGuard();

const editorRoot = document.querySelector("#editor-root");

if (!(editorRoot instanceof HTMLElement)) {
  throw new Error("Editor root element not found");
}

createRoot(editorRoot).render(
  <StrictMode>
    <ClientSupportGuard>
      <EditorShell />
    </ClientSupportGuard>
  </StrictMode>,
);
