import { useCallback, useEffect, useRef, useState } from "react";

import {
  type ApiError,
  createInvitation,
  generateInvitationToken,
  type Invitation,
  listInvitations,
  revokeInvitation,
  type Session,
} from "../auth/api";

// Safe, fixed messages: no server text, invitation ID, or token is shown.
const messages: Record<ApiError, string> = {
  bad_request: "The request was not accepted. Reload the page and try again.",
  unauthorized: "Your session has ended. Sign in again.",
  forbidden:
    "Only the canvas owner can invite moderators. Reload the page and try again.",
  not_found: "This invitation no longer exists. Reload the page and try again.",
  conflict: "The request changed while it was being retried. Try again.",
  identity_taken: "The service is unavailable. Try again.",
  rate_limited: "Too many invitations. Try again later.",
  subscription_inactive:
    "Your subscription is inactive. Ask an operator to reactivate it.",
  unavailable: "The service is unavailable. Try again.",
};

const expiryFormat = new Intl.DateTimeFormat("en", {
  dateStyle: "medium",
  timeStyle: "short",
});

type ListState =
  | { kind: "loading" }
  | { kind: "ready"; invitations: Invitation[] }
  | { kind: "unavailable"; message: string };

interface PendingCreation {
  operationId: string;
  token: string;
}

interface PendingRevocation {
  invitationId: string;
  operationId: string;
}

interface InvitationsPanelProps {
  session: Session;
  newOperationId?: () => string;
  newToken?: () => string;
}

export function InvitationsPanel({
  session,
  newOperationId = () => crypto.randomUUID(),
  newToken = generateInvitationToken,
}: InvitationsPanelProps) {
  const [list, setList] = useState<ListState>({ kind: "loading" });
  // The link lives only in this state: it is gone once the panel reloads
  // or another link is created, and the server never returns it.
  const [link, setLink] = useState<string | null>(null);
  const [copyNotice, setCopyNotice] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  // A creation keeps its operation ID and token, and a revocation its
  // operation ID, until a definitive answer, so a retry after a network
  // failure is replayed instead of repeated.
  const pendingCreation = useRef<PendingCreation | null>(null);
  const pendingRevocation = useRef<PendingRevocation | null>(null);
  const mounted = useRef(true);

  const reload = useCallback(async () => {
    const result = await listInvitations();
    if (!mounted.current) return;
    setList(
      result.ok
        ? { kind: "ready", invitations: result.value }
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

  function startAction() {
    setBusy(true);
    setError(null);
    setNotice(null);
    setCopyNotice(null);
  }

  async function create() {
    startAction();
    setLink(null);
    try {
      pendingCreation.current ??= {
        operationId: newOperationId(),
        token: newToken(),
      };
      const { operationId, token } = pendingCreation.current;
      const result = await createInvitation(
        session.csrfToken,
        operationId,
        token,
      );
      if (result.ok || result.error !== "unavailable") {
        pendingCreation.current = null;
      }
      if (!mounted.current) return;
      if (result.ok) {
        setLink(`${window.location.origin}/editor/#${token}`);
        void reload();
      } else {
        setError(messages[result.error]);
      }
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  async function revoke(invitationId: string) {
    startAction();
    try {
      if (pendingRevocation.current?.invitationId !== invitationId) {
        pendingRevocation.current = {
          invitationId,
          operationId: newOperationId(),
        };
      }
      const result = await revokeInvitation(
        session.csrfToken,
        pendingRevocation.current.operationId,
        invitationId,
      );
      if (result.ok || result.error !== "unavailable") {
        pendingRevocation.current = null;
      }
      if (!mounted.current) return;
      if (result.ok) {
        if (result.value.status === "accepted") {
          setNotice("This invitation was already accepted.");
        }
        void reload();
      } else {
        setError(messages[result.error]);
      }
    } finally {
      if (mounted.current) setBusy(false);
    }
  }

  function copy(value: string) {
    navigator.clipboard.writeText(value).then(
      () => mounted.current && setCopyNotice("Link copied."),
      () => mounted.current && setCopyNotice("Copy the link from the field."),
    );
  }

  return (
    <section aria-labelledby="invitations-heading">
      <h2 id="invitations-heading">Invite moderators</h2>
      <button type="button" disabled={busy} onClick={() => void create()}>
        Create invitation link
      </button>
      {link && (
        <div>
          <label>
            Invitation link
            <input
              type="text"
              readOnly
              value={link}
              autoComplete="off"
              spellCheck={false}
              onFocus={(event) => event.currentTarget.select()}
            />
          </label>
          <button type="button" onClick={() => copy(link)}>
            Copy link
          </button>
          <p>This link is shown only once. It expires in 7 days.</p>
          {copyNotice && <p role="status">{copyNotice}</p>}
        </div>
      )}
      {notice && <p role="status">{notice}</p>}
      {error && <p role="alert">{error}</p>}
      <h3>Pending invitations</h3>
      {list.kind === "loading" && <p role="status">Loading invitations…</p>}
      {list.kind === "unavailable" && <p role="alert">{list.message}</p>}
      {list.kind === "ready" &&
        (list.invitations.length === 0 ? (
          <p>No pending invitations.</p>
        ) : (
          <ul>
            {list.invitations.map((invitation) => (
              <li key={invitation.invitationId}>
                Expires{" "}
                <time dateTime={invitation.expiresAt}>
                  {expiryFormat.format(new Date(invitation.expiresAt))}
                </time>{" "}
                <button
                  type="button"
                  disabled={busy}
                  onClick={() => void revoke(invitation.invitationId)}
                >
                  Revoke
                </button>
              </li>
            ))}
          </ul>
        ))}
    </section>
  );
}
