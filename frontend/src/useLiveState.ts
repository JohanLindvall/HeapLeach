import { useEffect, useState } from 'react';
import { subscribeLiveState } from './liveState';
import type { ConnectionState, Snapshot } from './types';

/** Live snapshots with a polling fallback while the event stream is down. */
export function useLiveState(): { snapshot: Snapshot | null; connection: ConnectionState } {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [connection, setConnection] = useState<ConnectionState>('connecting');
  useEffect(() => subscribeLiveState(setSnapshot, setConnection), []);
  return { snapshot, connection };
}
