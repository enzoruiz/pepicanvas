import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { ClientSupportGuard } from "../client-support/ClientSupportGuard";
import { EditorShell } from "./EditorShell";

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
