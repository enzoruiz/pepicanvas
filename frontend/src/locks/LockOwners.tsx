import { useEffect, useState } from "react";

import { type LockEntry, listLocks } from "./api";

// How often the holders are read again. A lock lives 5 s without a
// heartbeat, so a released or expired lock disappears within a few seconds.
export const defaultLockPollMs = 2000;

interface LockOwnersProps {
  tenantId: string;
  canvasId: string;
  pollMs?: number;
}

function holderText({ holder }: LockEntry): string {
  if (holder.mine) return "Locked by you";
  return holder.name === ""
    ? "Locked by another participant"
    : `Locked by ${holder.name}`;
}

// The objects of a canvas that are locked and who holds each (US-020). The
// holder is named only by the account name the server shows. Nothing is
// shown while the holders cannot be read.
export function LockOwners({
  tenantId,
  canvasId,
  pollMs = defaultLockPollMs,
}: LockOwnersProps) {
  const [entries, setEntries] = useState<LockEntry[]>([]);

  useEffect(() => {
    let active = true;
    let reading = false;
    async function read() {
      if (reading) return;
      reading = true;
      const result = await listLocks({ tenantId, canvasId });
      reading = false;
      if (active) setEntries(result.ok ? result.value : []);
    }
    void read();
    const timer = setInterval(() => void read(), pollMs);
    return () => {
      active = false;
      clearInterval(timer);
    };
  }, [tenantId, canvasId, pollMs]);

  if (entries.length === 0) return null;
  return (
    <ul aria-label="Locked objects">
      {entries.map((entry) => (
        <li key={entry.objectId} data-object-id={entry.objectId}>
          {holderText(entry)}
        </li>
      ))}
    </ul>
  );
}
