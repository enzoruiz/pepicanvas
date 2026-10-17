import {
  useEffect,
  useState,
  type ReactNode,
  type SyntheticEvent,
} from "react";

import { readClientSnapshot } from "./clientSnapshot";
import { classifyClientSupport, type ClientSnapshot } from "./clientSupport";

interface ClientSupportGuardProps {
  children: ReactNode;
  readSnapshot?: () => ClientSnapshot;
}

const guardStyle = {
  minHeight: "100vh",
  position: "relative",
} as const;

const blockingLayerStyle = {
  alignItems: "center",
  background: "#ffffff",
  color: "#111827",
  display: "flex",
  inset: 0,
  justifyContent: "center",
  padding: "2rem",
  position: "fixed",
  textAlign: "center",
  zIndex: 2147483647,
} as const;

const explanationStyle = {
  maxWidth: "40rem",
} as const;

function blockInteraction(event: SyntheticEvent) {
  event.preventDefault();
  event.stopPropagation();
}

export function ClientSupportGuard({
  children,
  readSnapshot = readClientSnapshot,
}: ClientSupportGuardProps) {
  const [snapshot, setSnapshot] = useState(readSnapshot);

  useEffect(() => {
    const updateSnapshot = () => {
      setSnapshot(readSnapshot());
    };

    window.addEventListener("resize", updateSnapshot);
    return () => {
      window.removeEventListener("resize", updateSnapshot);
    };
  }, [readSnapshot]);

  const classification = classifyClientSupport(snapshot);
  const blocked = !classification.supported;

  return (
    <div style={guardStyle}>
      <div
        aria-hidden={blocked ? "true" : undefined}
        data-testid="client-support-content"
        inert={blocked ? true : undefined}
        onClickCapture={blocked ? blockInteraction : undefined}
        style={blocked ? { pointerEvents: "none" } : undefined}
      >
        {children}
      </div>

      {blocked ? (
        <section
          aria-labelledby="client-support-title"
          role="alert"
          style={blockingLayerStyle}
        >
          <div style={explanationStyle}>
            <h1 id="client-support-title">Unsupported client</h1>
            <p>
              PepiCanvas requires desktop Google Chrome, Microsoft Edge, or a
              supported OBS Browser Source, and a viewport of at least 1280 by
              720 pixels. Mobile and tablet devices are not supported.
            </p>
          </div>
        </section>
      ) : null}
    </div>
  );
}
