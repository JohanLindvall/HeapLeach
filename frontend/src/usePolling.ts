// SPDX-License-Identifier: MIT
import { useEffect, type DependencyList } from 'react';

export interface PollingOptions {
  /** Polling stops while this is false, and starts again when it is not. */
  readonly active?: boolean;
  /** The wait between the end of one poll and the start of the next. */
  readonly intervalMs: number;
  /** A poll taking longer than this is aborted; 0 or absent never is. */
  readonly timeoutMs?: number;
}

/**
 * Polls while the page is visible: at once, then intervalMs after each poll
 * ends, and at once again when a hidden page is shown. Never two at a time.
 *
 * poll is given a signal that is aborted when deps change, the component
 * goes, or the poll times out; it checks the signal before applying what it
 * fetched. onError hears a poll's failure, a timeout included, unless the
 * polling it belonged to has ended.
 */
export function usePolling(
  { active = true, intervalMs, timeoutMs = 0 }: PollingOptions,
  poll: (signal: AbortSignal) => Promise<void>,
  onError: (error: unknown) => void,
  deps: DependencyList,
): void {
  useEffect(() => {
    if (!active) return;
    let disposed = false;
    let timer: number | undefined;
    // The latest poll's controller, aborted with the polling even after the
    // poll has ended.
    let latest: AbortController | null = null;
    let pending = false;
    const refresh = async (): Promise<void> => {
      if (disposed || pending) return;
      window.clearTimeout(timer);
      if (!document.hidden) {
        const controller = new AbortController();
        latest = controller;
        pending = true;
        const timeout = timeoutMs > 0 ? window.setTimeout(() => controller.abort(), timeoutMs) : undefined;
        try {
          await poll(controller.signal);
        } catch (error) {
          if (!disposed) onError(error);
        } finally {
          window.clearTimeout(timeout);
          pending = false;
        }
      }
      if (!disposed) timer = window.setTimeout(() => { void refresh(); }, intervalMs);
    };
    const visible = (): void => { if (!document.hidden) void refresh(); };
    void refresh();
    document.addEventListener('visibilitychange', visible);
    return () => {
      disposed = true;
      latest?.abort();
      window.clearTimeout(timer);
      document.removeEventListener('visibilitychange', visible);
    };
    // The caller's deps say when polling starts over; poll and onError are
    // read as they were then.
  },[active, intervalMs, timeoutMs, ...deps]);
}
