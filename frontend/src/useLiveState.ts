import { useEffect, useState } from 'react';
import { subscribeLiveState } from './liveState';
import type { ConnectionState, Snapshot } from './types';

/**
 * Live snapshots with a polling fallback while the event stream is down.
 *
 * open is the comma-joined ids of the jobs the user has expanded, and the
 * subscription is keyed on it: everything else arrives with its items
 * reduced to what the queue-wide views read. It is a string rather than a
 * set because the effect has to re-run when the *contents* change and not
 * every time a snapshot rebuilds the list.
 */
export function useLiveState(open = ''): {
  snapshot: Snapshot | null;
  connection: ConnectionState;
} {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [connection, setConnection] = useState<ConnectionState>('connecting');
  useEffect(() => subscribeLiveState(setSnapshot, setConnection, open), [open]);
  return { snapshot, connection };
}
