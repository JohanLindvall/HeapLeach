import { fetchState } from './api';
import type { ConnectionState, Snapshot } from './types';

const RECONNECT_MIN_MS = 500;
const RECONNECT_MAX_MS = 10_000;
const POLL_INTERVAL_MS = 2000;
const POLL_TIMEOUT_MS = 10_000;

/** Owns one event stream and its polling fallback; returns a full cleanup. */
export function subscribeLiveState(
  onSnapshot: (snapshot: Snapshot) => void,
  onConnection: (connection: ConnectionState) => void,
): () => void {
  let source: EventSource | null = null;
  let reconnectTimer: number | undefined;
  let pollTimer: number | undefined;
  let pollRequest: AbortController | null = null;
  let retry = RECONNECT_MIN_MS;
  let polling = false;
  let closed = false;

  const stopPolling = (): void => {
    polling = false;
    window.clearTimeout(pollTimer);
    pollRequest?.abort();
    pollRequest = null;
  };

  const poll = async (): Promise<void> => {
    const request = new AbortController();
    pollRequest = request;
    const timeout = window.setTimeout(() => request.abort(), POLL_TIMEOUT_MS);
    try {
      const state = await fetchState(request.signal);
      // A poll can finish after the stream reconnects or the component
      // unmounts. It must never replace a newer stream's state.
      if (!closed && polling && !request.signal.aborted) onSnapshot(state);
    } catch {
      // Keep the last useful snapshot while the service is unavailable.
    } finally {
      window.clearTimeout(timeout);
      if (pollRequest === request) {
        pollRequest = null;
        // Schedule after completion so slow requests cannot pile up or
        // finish out of order as they can with a fixed interval.
        if (!closed && polling) pollTimer = window.setTimeout(() => void poll(), POLL_INTERVAL_MS);
      }
    }
  };

  const disconnected = (): void => {
    if (closed) return;
    onConnection('offline');
    if (!polling) {
      polling = true;
      void poll();
    }
    window.clearTimeout(reconnectTimer);
    reconnectTimer = window.setTimeout(connect, retry);
    retry = Math.min(retry * 2, RECONNECT_MAX_MS);
  };

  const connect = (): void => {
    if (closed) return;
    let stream: EventSource;
    try {
      stream = new EventSource('/api/events');
    } catch {
      // A browser without EventSource still has a working polling UI.
      disconnected();
      return;
    }
    source = stream;
    stream.onopen = (): void => {
      if (closed || source !== stream) return;
      retry = RECONNECT_MIN_MS;
      stopPolling();
      onConnection('live');
    };
    stream.onmessage = (event: MessageEvent<string>): void => {
      if (closed || source !== stream) return;
      try {
        onSnapshot(JSON.parse(event.data) as Snapshot);
      } catch {
        // Wait for the next complete frame if this one is malformed.
      }
    };
    stream.onerror = (): void => {
      if (closed || source !== stream) return;
      stream.close();
      source = null;
      disconnected();
    };
  };

  connect();
  return (): void => {
    closed = true;
    source?.close();
    source = null;
    window.clearTimeout(reconnectTimer);
    stopPolling();
  };
}
