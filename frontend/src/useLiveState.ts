import { useEffect, useState } from 'react';
import { subscribeLiveState } from './liveState';
import { mergeSnapshot } from './snapshotMerge';
import type { ConnectionState, Snapshot } from './types';

/** Live snapshots with a polling fallback while the event stream is down. */
export function useLiveState(): { snapshot: Snapshot | null; connection: ConnectionState } {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [connection, setConnection] = useState<ConnectionState>('connecting');
  useEffect(
    () =>
      subscribeLiveState(
        // A frame carries only the rows that moved; the rest is filled back
        // in from what is already held, so everything downstream sees
        // complete lists. Merging inside the updater is what gives it the
        // previous state to fill from, and it is idempotent, so React
        // replaying it changes nothing.
        (frame) => setSnapshot((previous) => mergeSnapshot(previous, frame)),
        setConnection,
      ),
    [],
  );
  return { snapshot, connection };
}
