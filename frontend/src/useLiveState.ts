import { useEffect, useRef, useState } from 'react';
import { fetchState } from './api';
import type { ConnectionState, Snapshot } from './types';

/** Backoff bounds for reconnecting a dropped event stream. */
const RECONNECT_MIN_MS = 500;
const RECONNECT_MAX_MS = 10_000;

/**
 * How often to poll while the stream is down. Slower than the stream's own
 * tick on purpose: this is a fallback, and the stylesheet lengthens the
 * progress-fill motion to match it (see .app--polling).
 */
const POLL_INTERVAL_MS = 2000;

/**
 * Subscribes to the server's event stream and returns the latest snapshot.
 *
 * The server pushes the entire state on every tick, so a missed message is
 * self-healing: the next one is authoritative. If EventSource cannot connect
 * at all, this falls back to polling so the page still works.
 */
export function useLiveState(): { snapshot: Snapshot | null; connection: ConnectionState } {
  const [snapshot, setSnapshot] = useState<Snapshot | null>(null);
  const [connection, setConnection] = useState<ConnectionState>('connecting');
  const retryRef = useRef(RECONNECT_MIN_MS);

  useEffect(() => {
    let source: EventSource | null = null;
    let reconnectTimer: number | undefined;
    let pollTimer: number | undefined;
    let closed = false;

    const connect = (): void => {
      if (closed) return;
      source = new EventSource('/api/events');

      source.onopen = (): void => {
        retryRef.current = RECONNECT_MIN_MS;
        setConnection('live');
        // The stream is healthy; stop the polling fallback.
        window.clearInterval(pollTimer);
        pollTimer = undefined;
      };

      source.onmessage = (event: MessageEvent<string>): void => {
        try {
          setSnapshot(JSON.parse(event.data) as Snapshot);
        } catch {
          // A malformed frame is not worth tearing the stream down for.
        }
      };

      source.onerror = (): void => {
        source?.close();
        source = null;
        if (closed) return;

        setConnection('offline');
        startPolling();

        reconnectTimer = window.setTimeout(connect, retryRef.current);
        retryRef.current = Math.min(retryRef.current * 2, RECONNECT_MAX_MS);
      };
    };

    // While the stream is down, keep the page current at a slower cadence.
    const startPolling = (): void => {
      if (pollTimer !== undefined) return;
      const poll = (): void => {
        void fetchState()
          .then((state) => setSnapshot(state))
          .catch(() => undefined);
      };
      poll();
      pollTimer = window.setInterval(poll, POLL_INTERVAL_MS);
    };

    connect();

    return (): void => {
      closed = true;
      source?.close();
      window.clearTimeout(reconnectTimer);
      window.clearInterval(pollTimer);
    };
  }, []);

  return { snapshot, connection };
}
