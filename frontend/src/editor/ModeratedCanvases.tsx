import { useEffect, useRef, useState } from "react";

import {
  type Canvas,
  getTenantCanvas,
  listMemberships,
  mediaContentUrl,
  type Membership,
  type Result,
} from "../auth/api";
import { CanvasSurface } from "../canvas/CanvasSurface";
import { LockOwners } from "../locks/LockOwners";

// Safe, fixed messages: no server text, tenant, or canvas ID is shown.
const listUnavailableMessage =
  "The canvases you moderate are unavailable. Reload the page to try again.";
const canvasUnavailableMessage =
  "This canvas is unavailable. Reload the page to try again.";
const refreshFailedMessage =
  "This canvas could not be refreshed. The last loaded version is shown. Try again.";
// The ended canvas is no longer listed, so the reason names no canvas.
const accessEndedMessage =
  "Your moderator access to a canvas has ended. It was removed from this list.";

interface Entry {
  membership: Membership;
  // The last answer, or the last loaded canvas when a refresh failed.
  canvas: Result<Canvas>;
  // A refresh failed without ending the access; canvas is the last good one.
  refreshFailed: boolean;
  // A refresh is in flight; its button is disabled.
  refreshing: boolean;
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
  // Set by a refused refresh, so the reload it asks for keeps the reason.
  // Any other load shows the reason only if one of its own reads is refused.
  const carryEnded = useRef(false);
  // Incremented by every list load; a refresh answered after a newer load
  // is ignored, so it cannot overwrite the newer data.
  const generation = useRef(0);
  // Canvases with a refresh in flight, checked before React re-renders.
  const inFlight = useRef(new Set<string>());

  useEffect(() => {
    let active = true;
    const current = ++generation.current;
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
      if (!active || current !== generation.current) return;
      const entries = read
        .filter(({ canvas }) => canvas.ok || canvas.error !== "forbidden")
        .map((entry) => ({
          ...entry,
          refreshFailed: false,
          refreshing: false,
        }));
      setEnded(entries.length < read.length || carryEnded.current);
      carryEnded.current = false;
      setState({ kind: "ready", entries });
      onListed?.(entries.length);
    })();
    return () => {
      active = false;
    };
  }, [refreshKey, reloads, onListed]);

  function update(membership: Membership, change: (entry: Entry) => Entry) {
    setState((current) =>
      current.kind === "ready"
        ? {
            kind: "ready",
            entries: current.entries.map((entry) =>
              sameCanvas(entry.membership, membership) ? change(entry) : entry,
            ),
          }
        : current,
    );
  }

  async function refresh(entry: Entry) {
    const { membership } = entry;
    const key = `${membership.tenantId}/${membership.canvasId}`;
    if (inFlight.current.has(key)) return;
    inFlight.current.add(key);
    const started = generation.current;
    update(membership, (current) => ({ ...current, refreshing: true }));
    const canvas = await getTenantCanvas(
      membership.tenantId,
      membership.canvasId,
    ).finally(() => inFlight.current.delete(key));
    if (started !== generation.current) return;
    if (!canvas.ok && canvas.error === "forbidden") {
      // A new load also discards any load in flight, so it cannot bring
      // the data back.
      setEnded(true);
      carryEnded.current = true;
      setState((current) =>
        current.kind === "ready"
          ? {
              kind: "ready",
              entries: current.entries.filter(
                (other) => !sameCanvas(other.membership, membership),
              ),
            }
          : current,
      );
      setReloads((count) => count + 1);
      return;
    }
    update(membership, (current) =>
      canvas.ok || !current.canvas.ok
        ? { membership, canvas, refreshFailed: false, refreshing: false }
        : { ...current, refreshFailed: true, refreshing: false },
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
        state.entries.map((entry) => {
          const { membership, canvas } = entry;
          return (
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
                      mediaSource={(content) =>
                        mediaContentUrl(
                          membership.tenantId,
                          membership.canvasId,
                          content,
                        )
                      }
                    />
                  </div>
                  <LockOwners
                    tenantId={canvas.value.tenantId}
                    canvasId={canvas.value.canvasId}
                  />
                </>
              ) : (
                <p role="alert">{canvasUnavailableMessage}</p>
              )}
              {entry.refreshFailed && (
                <p role="alert">{refreshFailedMessage}</p>
              )}
              <button
                type="button"
                disabled={entry.refreshing}
                onClick={() => void refresh(entry)}
              >
                Refresh canvas
              </button>
            </article>
          );
        })}
    </section>
  );
}
