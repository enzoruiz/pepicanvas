import { useEffect, useRef, useState } from "react";

import { acceptInvitation, type ApiError, type Session } from "../auth/api";
import { acceptOperationKey, clearInvitationFragment } from "./invitationLink";

// Safe, fixed messages: no server text, token, tenant, or canvas is shown.
const acceptedMessage = "You are now a moderator of this canvas.";
const unusableMessage =
  "This invitation link can't be used. Ask the streamer for a new one.";
const messages: Record<ApiError, string> = {
  bad_request: "The request was not accepted. Reload the page and try again.",
  unauthorized: "Your session has ended. Sign in again.",
  forbidden: "The request was rejected. Reload the page and try again.",
  not_found: unusableMessage,
  conflict: "The request changed while it was being retried. Try again.",
  identity_taken: "The service is unavailable. Try again.",
  rate_limited: "Too many attempts. Wait a few minutes and try again.",
  subscription_inactive: "The service is unavailable. Try again.",
  unavailable: "The service is unavailable. Try again.",
};

// Answers that end the operation, so a later attempt gets a new ID. Every
// other answer keeps the ID, and a retry or reload replays the first result.
const definitive = new Set<ApiError>(["not_found", "conflict", "bad_request"]);

type Outcome =
  | { kind: "pending" }
  | { kind: "accepted" }
  | { kind: "unusable" }
  | { kind: "failed"; message: string };

interface AcceptInvitationPanelProps {
  session: Session;
  token: string | null;
  newOperationId?: () => string;
  // Called once the invitation got a definitive accept or rejection.
  onSettled?: (accepted: boolean) => void;
}

function readStored(key: string): string | null {
  try {
    return window.sessionStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key: string, value: string | null) {
  try {
    if (value === null) {
      window.sessionStorage.removeItem(key);
    } else {
      window.sessionStorage.setItem(key, value);
    }
  } catch {
    // Without storage the operation ID lives only in this component.
  }
}

export function AcceptInvitationPanel({
  session,
  token,
  newOperationId = () => crypto.randomUUID(),
  onSettled,
}: AcceptInvitationPanelProps) {
  const [outcome, setOutcome] = useState<Outcome>({ kind: "pending" });
  const [busy, setBusy] = useState(false);
  // Fallback when sessionStorage is unavailable.
  const pending = useRef<string | null>(null);
  const mounted = useRef(true);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  async function accept(current: string) {
    setBusy(true);
    try {
      const key = await acceptOperationKey(current);
      // The operation ID survives a reload until a definitive answer, so a
      // resend after a lost answer replays it instead of repeating it.
      const operationId =
        readStored(key) ?? pending.current ?? newOperationId();
      pending.current = operationId;
      writeStored(key, operationId);
      const result = await acceptInvitation(
        session.csrfToken,
        operationId,
        current,
      );
      if (result.ok || definitive.has(result.error)) {
        pending.current = null;
        writeStored(key, null);
      }
      if (result.ok || result.error === "not_found") {
        clearInvitationFragment();
      }
      if (mounted.current) {
        if (result.ok) {
          setOutcome({ kind: "accepted" });
        } else if (result.error === "not_found") {
          setOutcome({ kind: "unusable" });
        } else {
          setOutcome({ kind: "failed", message: messages[result.error] });
        }
      }
      if (result.ok || result.error === "not_found") {
        onSettled?.(result.ok);
      }
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  if (outcome.kind === "accepted") {
    return (
      <section aria-labelledby="accept-invitation-heading">
        <h2 id="accept-invitation-heading">Moderator invitation</h2>
        <p role="status">{acceptedMessage}</p>
      </section>
    );
  }
  if (outcome.kind === "unusable") {
    return (
      <section aria-labelledby="accept-invitation-heading">
        <h2 id="accept-invitation-heading">Moderator invitation</h2>
        <p role="alert">{unusableMessage}</p>
      </section>
    );
  }
  if (token === null) return null;
  return (
    <section aria-labelledby="accept-invitation-heading">
      <h2 id="accept-invitation-heading">Moderator invitation</h2>
      <p>You were invited to moderate a streamer&apos;s canvas.</p>
      <button type="button" disabled={busy} onClick={() => void accept(token)}>
        Accept invitation
      </button>
      {outcome.kind === "failed" && <p role="alert">{outcome.message}</p>}
    </section>
  );
}
