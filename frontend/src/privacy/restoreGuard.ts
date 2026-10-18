interface RestoreGuardEnvironment {
  target: EventTarget;
  document: Document;
  reload: () => void;
}

function isPersisted(event: Event): boolean {
  return (event as PageTransitionEvent).persisted === true;
}

/**
 * Keeps private content out of the back/forward cache (TE-003).
 *
 * Browsers may restore `Cache-Control: no-store` pages from the back/forward
 * cache after a server-side access cut. The guard hides the document before
 * it is frozen and reloads it when restored, so the server decides again.
 */
export function installRestoreGuard({
  target,
  document,
  reload,
}: RestoreGuardEnvironment): void {
  target.addEventListener("pagehide", (event) => {
    if (isPersisted(event)) {
      document.documentElement.hidden = true;
    }
  });
  target.addEventListener("pageshow", (event) => {
    if (isPersisted(event)) {
      reload();
    }
  });
}

export function installWindowRestoreGuard(): void {
  installRestoreGuard({
    target: window,
    document,
    reload: () => window.location.reload(),
  });
}
