// SPDX-License-Identifier: MIT
import { useEffect, useRef, useState } from 'react';
import { ApiError, fetchLogs } from './api';
import { mergeLogs } from './logs';
import type { LogPage } from './types';

const REFRESH_MS = 1000;
const TIMEOUT_MS = 10_000;

/** Poll only while the log panel is visible, retaining the cursor on pause. */
export function useLogs(paused: boolean) {
  const [page, setPage] = useState<LogPage | null>(null);
  const [error, setError] = useState('');
  const cursor = useRef({ session: '', after: 0 });

  useEffect(() => {
    if (paused) return;
    let disposed = false;
    let timer: number | undefined;
    let pending: AbortController | null = null;
    const refresh = async (): Promise<void> => {
      if (disposed || pending) return;
      window.clearTimeout(timer);
      if (!document.hidden) {
        const controller = new AbortController();
        pending = controller;
        const timeout = window.setTimeout(() => controller.abort(), TIMEOUT_MS);
        try {
          const next = await fetchLogs(cursor.current.session, cursor.current.after, controller.signal);
          if (!disposed && !controller.signal.aborted) {
            cursor.current = { session: next.session, after: next.next };
            setPage((current) => mergeLogs(current, next));
            setError('');
          }
        } catch (err) {
          if (!disposed) setError(err instanceof ApiError ? err.message : 'Could not refresh service logs.');
        } finally {
          window.clearTimeout(timeout);
          pending = null;
        }
      }
      if (!disposed) timer = window.setTimeout(() => { void refresh(); }, REFRESH_MS);
    };
    const visible = (): void => { if (!document.hidden) void refresh(); };
    void refresh();
    document.addEventListener('visibilitychange', visible);
    return () => {
      disposed = true;
      pending?.abort();
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', visible);
    };
  }, [paused]);

  return { page, error };
}
