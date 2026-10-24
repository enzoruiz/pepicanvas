import { useState } from "react";

import { AuthGate } from "../auth/AuthGate";
import type { OwnCanvas, Session } from "../auth/api";
import { CanvasPanel } from "./CanvasPanel";
import { InvitationsPanel } from "./InvitationsPanel";
import { MediaPanel } from "./MediaPanel";
import { OperatorPanel } from "./OperatorPanel";

// Only an owner with an active subscription loads its own canvas, so the
// invitations and storage panels wait for it; moderators and inactive or
// operator-only accounts never see them.
function SignedInEditor({ session }: { session: Session }) {
  const [ownCanvas, setOwnCanvas] = useState<OwnCanvas | null>(null);
  return (
    <>
      <CanvasPanel session={session} onLoaded={setOwnCanvas} />
      {ownCanvas && <InvitationsPanel session={session} />}
      {ownCanvas && (
        <MediaPanel
          session={session}
          tenantId={ownCanvas.tenantId}
          canvasId={ownCanvas.canvasId}
        />
      )}
      {session.operator && <OperatorPanel session={session} />}
    </>
  );
}

export function EditorShell() {
  return (
    <main aria-label="PepiCanvas editor">
      <h1>PepiCanvas editor</h1>
      <AuthGate>{(session) => <SignedInEditor session={session} />}</AuthGate>
    </main>
  );
}
