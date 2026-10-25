import { useEffect, useState } from "react";

import {
  type Canvas,
  getTenantCanvas,
  listMemberships,
  type Membership,
  type Result,
} from "../auth/api";
import { CanvasSurface } from "../canvas/CanvasSurface";

// Safe, fixed messages: no server text, tenant, or canvas ID is shown.
const listUnavailableMessage =
  "The canvases you moderate are unavailable. Reload the page to try again.";
const canvasUnavailableMessage =
  "This canvas is unavailable. Reload the page to try again.";

interface Entry {
  membership: Membership;
  canvas: Result<Canvas>;
}

type State =
  | { kind: "loading" }
  | { kind: "ready"; entries: Entry[] }
  | { kind: "unavailable" };

interface ModeratedCanvasesProps {
  // A new value loads the list again, for example after an acceptance.
  refreshKey?: number;
  // Reports how many moderated canvases were listed.
  onListed?: (count: number) => void;
}

// The canvases the account moderates (US-005 S17). Each one is read through
// the tenant route, which reauthorizes the membership on every read, and is
// shown read-only: owner settings stay in the owner's panels.
export function ModeratedCanvases({
  refreshKey = 0,
  onListed,
}: ModeratedCanvasesProps) {
  const [state, setState] = useState<State>({ kind: "loading" });

  useEffect(() => {
    let active = true;
    void (async () => {
      const list = await listMemberships();
      if (!list.ok) {
        if (active) setState({ kind: "unavailable" });
        return;
      }
      const entries = await Promise.all(
        list.value.map(async (membership) => ({
          membership,
          canvas: await getTenantCanvas(
            membership.tenantId,
            membership.canvasId,
          ),
        })),
      );
      if (!active) return;
      setState({ kind: "ready", entries });
      onListed?.(entries.length);
    })();
    return () => {
      active = false;
    };
  }, [refreshKey, onListed]);

  if (state.kind === "loading") return null;
  if (state.kind === "ready" && state.entries.length === 0) return null;
  return (
    <section aria-labelledby="moderated-canvases-heading">
      <h2 id="moderated-canvases-heading">Canvases you moderate</h2>
      {state.kind === "unavailable" && (
        <p role="alert">{listUnavailableMessage}</p>
      )}
      {state.kind === "ready" &&
        state.entries.map(({ membership, canvas }) => (
          <article
            key={`${membership.tenantId}/${membership.canvasId}`}
            aria-label="Moderated canvas"
          >
            {canvas.ok ? (
              <>
                <p>
                  {canvas.value.width} × {canvas.value.height}
                </p>
                <div style={{ maxWidth: "100%", overflow: "auto" }}>
                  <CanvasSurface
                    width={canvas.value.width}
                    height={canvas.value.height}
                    objects={canvas.value.objects}
                  />
                </div>
              </>
            ) : (
              <p role="alert">{canvasUnavailableMessage}</p>
            )}
          </article>
        ))}
    </section>
  );
}
