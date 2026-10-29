import { useCallback, useEffect, useRef, useState } from "react";

import {
  type ApiError,
  listMembers,
  type Member,
  revokeMember,
  type Session,
} from "../auth/api";

// Safe, fixed messages: no server text, account ID, or tenant ID is shown.
const messages: Record<ApiError, string> = {
  bad_request: "The request was not accepted. Reload the page and try again.",
  unauthorized: "Your session has ended. Sign in again.",
  forbidden:
    "Only the canvas owner can revoke moderators. Reload the page and try again.",
  not_found: "That moderator no longer has access.",
  conflict: "The request changed while it was being retried. Try again.",
  identity_taken: "The service is unavailable. Try again.",
  rate_limited: "Too many requests. Try again later.",
  subscription_inactive:
    "Your subscription is inactive. Ask an operator to reactivate it.",
  unavailable: "The service is unavailable. Try again.",
};

const revokedMessage =
  "Moderator access revoked. Their confirmed changes are kept.";

const acceptedFormat = new Intl.DateTimeFormat("en", {
  dateStyle: "medium",
  timeStyle: "short",
});

type ListState =
  | { kind: "loading" }
  | { kind: "ready"; members: Member[] }
  | { kind: "unavailable"; message: string };

interface ModeratorsPanelProps {
  session: Session;
  newOperationId?: () => string;
}

// The owner's moderators (US-006). A revocation is confirmed explicitly and
// cuts the moderator's access once the server answers.
export function ModeratorsPanel({
  session,
  newOperationId = () => crypto.randomUUID(),
}: ModeratorsPanelProps) {
  const [list, setList] = useState<ListState>({ kind: "loading" });
  const [confirming, setConfirming] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // Each account keeps its revocation's operation ID until a definitive
  // answer, so a retry after a network failure is replayed, not repeated.
  const pending = useRef(new Map<string, string>());
  const mounted = useRef(true);

  const reload = useCallback(async () => {
    const result = await listMembers();
    if (!mounted.current) return;
    setList(
      result.ok
        ? { kind: "ready", members: result.value }
        : { kind: "unavailable", message: messages[result.error] },
    );
  }, []);

  useEffect(() => {
    mounted.current = true;
    void reload();
    return () => {
      mounted.current = false;
    };
  }, [reload]);

  function ask(accountId: string) {
    setNotice(null);
    setError(null);
    setConfirming(accountId);
  }

  async function revoke(accountId: string) {
    setBusy(true);
    setNotice(null);
    setError(null);
    try {
      let operationId = pending.current.get(accountId);
      if (operationId === undefined) {
        operationId = newOperationId();
        pending.current.set(accountId, operationId);
      }
      const result = await revokeMember(
        session.csrfToken,
        operationId,
        accountId,
      );
      if (result.ok || result.error !== "unavailable") {
        pending.current.delete(accountId);
      }
      if (!mounted.current) return;
      if (result.ok) {
        setConfirming(null);
        setNotice(revokedMessage);
        setList((current) =>
          current.kind === "ready"
            ? {
                kind: "ready",
                members: current.members.filter(
                  (member) => member.accountId !== accountId,
                ),
              }
            : current,
        );
      } else if (result.error === "not_found") {
        setConfirming(null);
        setNotice(messages.not_found);
        void reload();
      } else {
        if (result.error !== "unavailable") setConfirming(null);
        setError(messages[result.error]);
      }
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  return (
    <section aria-labelledby="moderators-heading">
      <h2 id="moderators-heading">Moderators</h2>
      {notice && <p role="status">{notice}</p>}
      {error && <p role="alert">{error}</p>}
      {list.kind === "loading" && <p role="status">Loading moderators…</p>}
      {list.kind === "unavailable" && <p role="alert">{list.message}</p>}
      {list.kind === "ready" &&
        (list.members.length === 0 ? (
          <p>No moderators yet.</p>
        ) : (
          <ul>
            {list.members.map((member) => (
              <li key={member.accountId}>
                <span>{member.email}</span>{" "}
                {member.acceptedAt ? (
                  <>
                    Accepted{" "}
                    <time dateTime={member.acceptedAt.toISOString()}>
                      {acceptedFormat.format(member.acceptedAt)}
                    </time>
                  </>
                ) : (
                  "Acceptance date not recorded"
                )}{" "}
                {confirming === member.accountId ? (
                  <>
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => void revoke(member.accountId)}
                    >
                      Confirm revoke
                    </button>{" "}
                    <button
                      type="button"
                      disabled={busy}
                      onClick={() => setConfirming(null)}
                    >
                      Cancel
                    </button>
                  </>
                ) : (
                  <button
                    type="button"
                    disabled={busy}
                    onClick={() => ask(member.accountId)}
                  >
                    Revoke
                  </button>
                )}
              </li>
            ))}
          </ul>
        ))}
    </section>
  );
}
