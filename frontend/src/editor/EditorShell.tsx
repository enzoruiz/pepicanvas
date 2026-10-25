import { useCallback, useState } from "react";

import { AuthGate } from "../auth/AuthGate";
import type { OwnCanvas, Session } from "../auth/api";
import { AcceptInvitationPanel } from "./AcceptInvitationPanel";
import { CanvasPanel } from "./CanvasPanel";
import { InvitationsPanel } from "./InvitationsPanel";
import { readInvitationToken } from "./invitationLink";
import { ModeratedCanvases } from "./ModeratedCanvases";
import { OperatorPanel } from "./OperatorPanel";

const invitationNote =
  "Sign in or create an account to accept the moderator invitation.";

interface SignedInEditorProps {
  session: Session;
  invitationToken: string | null;
  onInvitationSettled: () => void;
}

// Only an owner with an active subscription loads its own canvas, so the
// invitations panel waits for it; moderators and inactive or operator-only
// accounts never see the panel. Moderated canvases are read-only.
function SignedInEditor({
  session,
  invitationToken,
  onInvitationSettled,
}: SignedInEditorProps) {
  const [ownCanvas, setOwnCanvas] = useState<OwnCanvas | null>(null);
  const [membershipsVersion, setMembershipsVersion] = useState(0);
  const [moderates, setModerates] = useState(false);
  const onListed = useCallback((count: number) => {
    setModerates(count > 0);
  }, []);
  return (
    <>
      <AcceptInvitationPanel
        session={session}
        token={invitationToken}
        onSettled={(accepted) => {
          onInvitationSettled();
          if (accepted) setMembershipsVersion((version) => version + 1);
        }}
      />
      <CanvasPanel
        session={session}
        onLoaded={setOwnCanvas}
        hideWhenNotOwner={moderates}
      />
      {ownCanvas && <InvitationsPanel session={session} />}
      <ModeratedCanvases refreshKey={membershipsVersion} onListed={onListed} />
      {session.operator && <OperatorPanel session={session} />}
    </>
  );
}

export function EditorShell() {
  // The invitation token is read once from the fragment; without a session
  // only the sign-in forms render, so nothing else is requested (US-005 S1).
  const [invitationToken, setInvitationToken] = useState(() =>
    readInvitationToken(window.location.hash),
  );
  return (
    <main aria-label="PepiCanvas editor">
      <h1>PepiCanvas editor</h1>
      <AuthGate note={invitationToken ? invitationNote : undefined}>
        {(session) => (
          <SignedInEditor
            session={session}
            invitationToken={invitationToken}
            onInvitationSettled={() => setInvitationToken(null)}
          />
        )}
      </AuthGate>
    </main>
  );
}
