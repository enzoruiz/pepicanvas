import { AuthGate } from "../auth/AuthGate";
import { CanvasPanel } from "./CanvasPanel";
import { OperatorPanel } from "./OperatorPanel";

export function EditorShell() {
  return (
    <main aria-label="PepiCanvas editor">
      <h1>PepiCanvas editor</h1>
      <AuthGate>
        {(session) => (
          <>
            <CanvasPanel session={session} />
            {session.operator && <OperatorPanel session={session} />}
          </>
        )}
      </AuthGate>
    </main>
  );
}
