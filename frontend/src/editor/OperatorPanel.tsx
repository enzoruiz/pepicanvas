import { type FormEvent, useRef, useState } from "react";

import {
  activate,
  type ApiError,
  deactivate,
  type Result,
  type Session,
} from "../auth/api";

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
  subscription_inactive: "The service is unavailable. Try again.",
  unavailable: "The service is unavailable. Try again.",
};

// A deactivation also answers not_found for an account that owns no streamer.
const deactivationMessages: Record<ApiError, string> = {
  ...messages,
  not_found: "No streamer has that email.",
};

type Action = "activate" | "deactivate";

interface PendingOperation {
  action: Action;
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
  // An operation keeps its ID until it gets a definitive answer, so a retry
  // of the same action and email after a network failure is replayed instead
  // of repeated. Another action or email is a new intent with a new ID.
  const pending = useRef<PendingOperation | null>(null);

  async function run(action: Action) {
    setBusy(true);
    setError(null);
    setNotice(null);
    try {
      if (
        pending.current?.action !== action ||
        pending.current.email !== email
      ) {
        pending.current = { action, email, operationId: newOperationId() };
      }
      const { operationId } = pending.current;
      let result: Result<string>;
      if (action === "activate") {
        const answer = await activate(session.csrfToken, operationId, email);
        result = answer.ok
          ? {
              ok: true,
              value: answer.value.created
                ? "Streamer activated."
                : "Streamer is active; the canvas is unchanged.",
            }
          : answer;
      } else {
        const answer = await deactivate(session.csrfToken, operationId, email);
        result = answer.ok
          ? {
              ok: true,
              value: answer.value.deactivated
                ? "Streamer deactivated. Their canvas and files are kept."
                : "Streamer was already inactive; nothing changed.",
            }
          : answer;
      }
      if (result.ok || result.error !== "unavailable") {
        pending.current = null;
      }
      if (result.ok) {
        setEmail("");
        setNotice(result.value);
      } else {
        setError(
          (action === "activate" ? messages : deactivationMessages)[
            result.error
          ],
        );
      }
    } finally {
      setBusy(false);
    }
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    void run("activate");
  }

  return (
    <section aria-labelledby="operator-heading">
      <h2 id="operator-heading">Manage streamers</h2>
      <form onSubmit={submit} noValidate>
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
        <button
          type="button"
          disabled={busy}
          onClick={() => void run("deactivate")}
        >
          Deactivate
        </button>
      </form>
      {notice && <p role="status">{notice}</p>}
      {error && <p role="alert">{error}</p>}
    </section>
  );
}
