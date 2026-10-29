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
const accessEndedMessage = "Your moderator access to this canvas has ended.";

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

function sameCanvas(a: Membership, b: Membership) {
  return a.tenantId === b.tenantId && a.canvasId === b.canvasId;
}

// The canvases the account moderates (US-005 S17). Each one is read through
// the tenant route, which reauthorizes the membership on every read, and is
// shown read-only: owner settings stay in the owner's panels. A read refused
// with forbidden means the access ended (US-006): the canvas's data is
// dropped at once, the list is read again, and only a fixed reason remains.
// Nothing is kept across reloads.
export function ModeratedCanvases({
  refreshKey = 0,
  onListed,
}: ModeratedCanvasesProps) {
  const [state, setState] = useState<State>({ kind: "loading" });
  const [ended, setEnded] = useState(false);
  // Bumped when a refused read asks for the list again.
  const [reloads, setReloads] = useState(0);

  useEffect(() => {
    let active = true;
    void (async () => {
      const list = await listMemberships();
      if (!list.ok) {
        if (active) setState({ kind: "unavailable" });
        return;
      }
      const read = await Promise.all(
        list.value.map(async (membership) => ({
          membership,
          canvas: await getTenantCanvas(
            membership.tenantId,
            membership.canvasId,
          ),
        })),
      );
      if (!active) return;
      const entries = read.filter(
        ({ canvas }) => canvas.ok || canvas.error !== "forbidden",
      );
      if (entries.length < read.length) setEnded(true);
      setState({ kind: "ready", entries });
      onListed?.(entries.length);
    })();
    return () => {
      active = false;
    };
  }, [refreshKey, reloads, onListed]);

  async function refresh(membership: Membership) {
    const canvas = await getTenantCanvas(
      membership.tenantId,
      membership.canvasId,
    );
    if (!canvas.ok && canvas.error === "forbidden") {
      // A new load also discards any load in flight, so it cannot bring
      // the data back.
      setEnded(true);
      setState((current) =>
        current.kind === "ready"
          ? {
              kind: "ready",
              entries: current.entries.filter(
                (entry) => !sameCanvas(entry.membership, membership),
              ),
            }
          : current,
      );
      setReloads((count) => count + 1);
      return;
    }
    setState((current) =>
      current.kind === "ready"
        ? {
            kind: "ready",
            entries: current.entries.map((entry) =>
              sameCanvas(entry.membership, membership)
                ? { membership, canvas }
                : entry,
            ),
          }
        : current,
    );
  }

  if (state.kind === "loading" && !ended) return null;
  if (state.kind === "ready" && state.entries.length === 0 && !ended) {
    return null;
  }
  return (
    <section aria-labelledby="moderated-canvases-heading">
      <h2 id="moderated-canvases-heading">Canvases you moderate</h2>
      {ended && <p role="alert">{accessEndedMessage}</p>}
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
            <button type="button" onClick={() => void refresh(membership)}>
              Refresh canvas
            </button>
          </article>
        ))}
    </section>
  );
}
