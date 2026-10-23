import { type FormEvent, useEffect, useRef, useState } from "react";

import {
  type ApiError,
  getOwnCanvas,
  type OwnCanvas,
  type Session,
  setDimensions,
} from "../auth/api";
import { CanvasSurface } from "../canvas/CanvasSurface";

// Safe, fixed messages: the panel shows only the signed-in account's canvas.
const inactiveMessage =
  "Your account is not active yet. Ask an operator to activate it.";
const unavailableMessage =
  "Your canvas is unavailable. Reload the page to try again.";

// The inclusive limits enforced by the server (US-007).
const widthLimits = { min: 320, max: 3840 };
const heightLimits = { min: 180, max: 2160 };
const widthMessage = "Width must be a whole number from 320 to 3840.";
const heightMessage = "Height must be a whole number from 180 to 2160.";

const saveFailedMessage = "The size could not be saved. Try again.";
const saveMessages: Partial<Record<ApiError, string>> = {
  bad_request: "That size is not allowed.",
  forbidden: "Only the canvas owner can change its size.",
  conflict: "That change was already used. Try again.",
};

type State =
  | { kind: "loading" }
  | { kind: "ready"; canvas: OwnCanvas }
  | { kind: "inactive" }
  | { kind: "unavailable" };

interface PendingResize {
  width: number;
  height: number;
  operationId: string;
}

interface CanvasPanelProps {
  session: Session;
  newOperationId?: () => string;
}

// A whole number within the limits, or undefined for anything else
// (empty, fractional, non-numeric, or out of range).
function parseSize(
  text: string,
  limits: { min: number; max: number },
): number | undefined {
  const trimmed = text.trim();
  if (!/^\d+$/.test(trimmed)) return undefined;
  const value = Number(trimmed);
  return value >= limits.min && value <= limits.max ? value : undefined;
}

export function CanvasPanel({
  session,
  newOperationId = () => crypto.randomUUID(),
}: CanvasPanelProps) {
  const [state, setState] = useState<State>({ kind: "loading" });
  const [width, setWidth] = useState("");
  const [height, setHeight] = useState("");
  const [errors, setErrors] = useState<string[]>([]);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // A resize keeps its operation ID until it gets a definitive answer, so a
  // retry after a network failure is replayed instead of repeated.
  const pending = useRef<PendingResize | null>(null);

  useEffect(() => {
    let active = true;
    void getOwnCanvas().then((result) => {
      if (!active) return;
      if (result.ok) {
        setState({ kind: "ready", canvas: result.value });
        setWidth(String(result.value.width));
        setHeight(String(result.value.height));
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

  async function submit(event: FormEvent<HTMLFormElement>, canvas: OwnCanvas) {
    event.preventDefault();
    setNotice(null);
    const newWidth = parseSize(width, widthLimits);
    const newHeight = parseSize(height, heightLimits);
    if (newWidth === undefined || newHeight === undefined) {
      setErrors([
        ...(newWidth === undefined ? [widthMessage] : []),
        ...(newHeight === undefined ? [heightMessage] : []),
      ]);
      return;
    }
    setErrors([]);
    setBusy(true);
    try {
      if (
        pending.current?.width !== newWidth ||
        pending.current.height !== newHeight
      ) {
        pending.current = {
          width: newWidth,
          height: newHeight,
          operationId: newOperationId(),
        };
      }
      const result = await setDimensions(
        session.csrfToken,
        canvas.tenantId,
        canvas.canvasId,
        pending.current.operationId,
        newWidth,
        newHeight,
      );
      if (result.ok || result.error !== "unavailable") {
        pending.current = null;
      }
      if (result.ok) {
        const {
          width: confirmedWidth,
          height: confirmedHeight,
          revision,
        } = result.value;
        setState({
          kind: "ready",
          canvas: {
            ...canvas,
            width: confirmedWidth,
            height: confirmedHeight,
            revision,
          },
        });
        setWidth(String(confirmedWidth));
        setHeight(String(confirmedHeight));
        setNotice("Canvas size saved.");
      } else {
        setErrors([saveMessages[result.error] ?? saveFailedMessage]);
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="canvas-heading">
      <h2 id="canvas-heading">Your canvas</h2>
      {state.kind === "loading" && <p role="status">Loading your canvas…</p>}
      {state.kind === "ready" && (
        <>
          <p>
            {state.canvas.width} × {state.canvas.height}
          </p>
          <div style={{ maxWidth: "100%", overflow: "auto" }}>
            <CanvasSurface
              width={state.canvas.width}
              height={state.canvas.height}
              objects={state.canvas.objects}
            />
          </div>
          <form
            aria-labelledby="canvas-size-heading"
            onSubmit={(event) => void submit(event, state.canvas)}
            noValidate
          >
            <h3 id="canvas-size-heading">Canvas size</h3>
            <label>
              Width
              <input
                type="text"
                inputMode="numeric"
                autoComplete="off"
                value={width}
                onChange={(event) => setWidth(event.target.value)}
                required
              />
            </label>
            <label>
              Height
              <input
                type="text"
                inputMode="numeric"
                autoComplete="off"
                value={height}
                onChange={(event) => setHeight(event.target.value)}
                required
              />
            </label>
            <button type="submit" disabled={busy}>
              Apply size
            </button>
          </form>
          {notice && <p role="status">{notice}</p>}
          {errors.length > 0 && <p role="alert">{errors.join(" ")}</p>}
        </>
      )}
      {state.kind === "inactive" && <p role="status">{inactiveMessage}</p>}
      {state.kind === "unavailable" && <p role="alert">{unavailableMessage}</p>}
    </section>
  );
}
