import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { EditorShell } from "./EditorShell";

const editorRoot = document.querySelector("#editor-root");

if (!(editorRoot instanceof HTMLElement)) {
  throw new Error("Editor root element not found");
}

createRoot(editorRoot).render(
  <StrictMode>
    <EditorShell />
  </StrictMode>,
);
