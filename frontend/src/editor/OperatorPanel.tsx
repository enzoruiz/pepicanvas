import { type FormEvent, useRef, useState } from "react";

import { activate, type ApiError, type Session } from "../auth/api";

// Safe, fixed messages: no server text or other account's data is shown.
const messages: Record<ApiError, string> = {
  bad_request: "Enter a valid email address.",
  unauthorized: "Your session has ended. Sign in again.",
  forbidden:
    "Only operators can activate streamers. Reload the page and try again.",
  not_found: "No account uses this email.",
  conflict: "The request changed while it was being retried. Try again.",
  identity_taken: "The service is unavailable. Try again.",
  rate_limited: "Too many attempts. Wait a few minutes and try again.",
  unavailable: "The service is unavailable. Try again.",
};

interface PendingActivation {
  email: string;
  operationId: string;
}

interface OperatorPanelProps {
  session: Session;
  newOperationId?: () => string;
}

export function OperatorPanel({
  session,
  newOperationId = () => crypto.randomUUID(),
}: OperatorPanelProps) {
  const [email, setEmail] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // An activation keeps its operation ID until it gets a definitive answer,
  // so a retry after a network failure is replayed instead of repeated.
  const pending = useRef<PendingActivation | null>(null);

  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      if (pending.current?.email !== email) {
        pending.current = { email, operationId: newOperationId() };
      }
      const result = await activate(
        session.csrfToken,
        pending.current.operationId,
        email,
      );
      if (result.ok || result.error !== "unavailable") {
        pending.current = null;
      }
      if (result.ok) {
        setEmail("");
        setNotice(
          result.value.created
            ? "Streamer activated."
            : "Streamer was already active; the canvas is unchanged.",
        );
      } else {
        setError(messages[result.error]);
      }
    } finally {
      setBusy(false);
    }
  }

  return (
    <section aria-labelledby="operator-heading">
      <h2 id="operator-heading">Activate a streamer</h2>
      <form onSubmit={(event) => void submit(event)} noValidate>
        <label>
          Streamer email
          <input
            type="email"
            autoComplete="off"
            value={email}
            onChange={(event) => setEmail(event.target.value)}
            required
          />
        </label>
        <button type="submit" disabled={busy}>
          Activate
        </button>
      </form>
      {notice && <p role="status">{notice}</p>}
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
