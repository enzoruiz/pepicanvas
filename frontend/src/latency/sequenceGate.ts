// The probe stream replays retained events after a reconnection, and events
// arrive in sequence order, so an event at or below the last sequence the
// page took is a repetition: it must not render or record a sample again.

export function createSequenceGate(): (sequence: number) => boolean {
  let last = 0;
  return (sequence) => {
    if (sequence <= last) return false;
    last = sequence;
    return true;
  };
}
