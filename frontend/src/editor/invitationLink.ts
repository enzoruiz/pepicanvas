// The moderator invitation link (US-005) carries its token in the fragment,
// which browsers never send to the server. The token leaves the page only in
// the JSON body of the accept request; it is never stored, logged, or shown.

// Same shape as generateInvitationToken: pci_ plus 43 base64url characters.
const fragmentPattern = /^#(pci_[A-Za-z0-9_-]{43})$/;

export function readInvitationToken(hash: string): string | null {
  return fragmentPattern.exec(hash)?.[1] ?? null;
}

// Drops the token from the address bar and the current history entry.
export function clearInvitationFragment(): void {
  window.history.replaceState(
    null,
    "",
    window.location.pathname + window.location.search,
  );
}

// The sessionStorage key for a pending acceptance's operation ID: derived
// from the token's SHA-256, so the token itself never reaches storage.
export async function acceptOperationKey(token: string): Promise<string> {
  const digest = await crypto.subtle.digest(
    "SHA-256",
    new TextEncoder().encode(token),
  );
  const hex = Array.from(new Uint8Array(digest), (byte) =>
    byte.toString(16).padStart(2, "0"),
  ).join("");
  return `pepicanvas.invitation-accept.${hex}`;
}
