import { AuthGate } from "../auth/AuthGate";

export function EditorShell() {
  return (
    <main aria-label="PepiCanvas editor">
      <h1>PepiCanvas editor</h1>
      <AuthGate>{() => null}</AuthGate>
    </main>
  );
}
