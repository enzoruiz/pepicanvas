import { useEffect, useState } from "react";

import { getOwnCanvas, type OwnCanvas } from "../auth/api";

// Safe, fixed messages: the panel shows only the signed-in account's canvas.
const inactiveMessage =
  "Your account is not active yet. Ask an operator to activate it.";
const unavailableMessage =
  "Your canvas is unavailable. Reload the page to try again.";

type State =
  | { kind: "loading" }
  | { kind: "ready"; canvas: OwnCanvas }
  | { kind: "inactive" }
  | { kind: "unavailable" };

export function CanvasPanel() {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    let active = true;
    void getOwnCanvas().then((result) => {
      if (!active) return;
      if (result.ok) {
        setState({ kind: "ready", canvas: result.value });
      } else {
        setState({
          kind: result.error === "forbidden" ? "inactive" : "unavailable",
        });
      }
    });
    return () => {
      active = false;
    };
  }, []);

  return (
    <section aria-labelledby="canvas-heading">
      <h2 id="canvas-heading">Your canvas</h2>
      {state.kind === "loading" && <p role="status">Loading your canvas…</p>}
      {state.kind === "ready" && (
        <p>
          {state.canvas.width} × {state.canvas.height}
        </p>
      )}
      {state.kind === "inactive" && <p role="status">{inactiveMessage}</p>}
      {state.kind === "unavailable" && <p role="alert">{unavailableMessage}</p>}
    </section>
  );
}
