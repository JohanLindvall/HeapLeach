// SPDX-License-Identifier: MIT
import { useRef, useState } from 'react';
import { errorMessage, fetchLogs } from './api';
import { mergeLogs } from './logs';
import type { LogPage } from './types';
import { usePolling } from './usePolling';

const REFRESH_MS = 1000;
const TIMEOUT_MS = 10_000;

/** Poll only while the log panel is visible, retaining the cursor on pause. */
export function useLogs(paused: boolean) {
  const [page, setPage] = useState<LogPage | null>(null);
  const [error, setError] = useState('');
  const cursor = useRef({ session: '', after: 0 });

  usePolling({ active: !paused, intervalMs: REFRESH_MS, timeoutMs: TIMEOUT_MS }, async (signal) => {
    const next = await fetchLogs(cursor.current.session, cursor.current.after, signal);
    if (signal.aborted) return;
    cursor.current = { session: next.session, after: next.next };
    setPage((current) => mergeLogs(current, next));
    setError('');
  }, (e) => setError(errorMessage(e, 'Could not refresh service logs.')), []);

  return { page, error };
}
