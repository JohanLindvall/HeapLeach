import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fetchState } from './api';
import { subscribeLiveState } from './liveState';
import type { Snapshot } from './types';

vi.mock('./api', () => ({ fetchState: vi.fn() }));

class Stream {
  static instances: Stream[] = [];
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  close = vi.fn();
  constructor() { Stream.instances.push(this); }
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => { resolve = yes; });
  return { promise, resolve };
}

const snapshot = { jobs: [], speed: 1 } as unknown as Snapshot;
let cleanup: (() => void) | undefined;

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('window', globalThis);
  vi.stubGlobal('EventSource', Stream);
  Stream.instances = [];
  vi.mocked(fetchState).mockReset();
});

afterEach(async () => {
  cleanup?.();
  cleanup = undefined;
  // Let aborted requests settle before removing their browser globals.
  await vi.advanceTimersByTimeAsync(0);
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

describe('live state', () => {
  it('discards an outstanding poll when the stream recovers', async () => {
    const pending = deferred<Snapshot>();
    vi.mocked(fetchState).mockReturnValue(pending.promise);
    const onSnapshot = vi.fn();
    const onConnection = vi.fn();
    cleanup = subscribeLiveState(onSnapshot, onConnection);
    Stream.instances[0]!.onerror!();
    const signal = vi.mocked(fetchState).mock.calls[0]![0]!;
    await vi.advanceTimersByTimeAsync(500);
    const stream = Stream.instances[1]!;
    stream.onopen!();
    stream.onmessage!({ data: JSON.stringify(snapshot) });
    expect(signal.aborted).toBe(true);
    pending.resolve({ ...snapshot, speed: 0 });
    await vi.advanceTimersByTimeAsync(0);
    expect(onSnapshot.mock.calls).toEqual([[snapshot]]);
    expect(onConnection.mock.calls).toEqual([['offline'], ['live']]);
  });

  it('keeps one poll in flight and schedules the next after completion', async () => {
    const pending = deferred<Snapshot>();
    vi.mocked(fetchState).mockReturnValue(pending.promise);
    cleanup = subscribeLiveState(vi.fn(), vi.fn());
    Stream.instances[0]!.onerror!();
    await vi.advanceTimersByTimeAsync(6000);
    expect(fetchState).toHaveBeenCalledTimes(1);
    pending.resolve(snapshot);
    await vi.advanceTimersByTimeAsync(1999);
    expect(fetchState).toHaveBeenCalledTimes(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(fetchState).toHaveBeenCalledTimes(2);
  });

  it('aborts polling and ignores late events after cleanup', async () => {
    const pending = deferred<Snapshot>();
    vi.mocked(fetchState).mockReturnValue(pending.promise);
    const onSnapshot = vi.fn();
    cleanup = subscribeLiveState(onSnapshot, vi.fn());
    const stream = Stream.instances[0]!;
    stream.onerror!();
    const signal = vi.mocked(fetchState).mock.calls[0]![0]!;
    cleanup();
    expect(signal.aborted).toBe(true);
    pending.resolve(snapshot);
    stream.onmessage!({ data: JSON.stringify(snapshot) });
    await vi.advanceTimersByTimeAsync(20_000);
    expect(onSnapshot).not.toHaveBeenCalled();
    expect(Stream.instances).toHaveLength(1);
    expect(fetchState).toHaveBeenCalledTimes(1);
  });

  it('polls even when EventSource is unavailable', async () => {
    vi.stubGlobal('EventSource', undefined);
    vi.mocked(fetchState).mockResolvedValue(snapshot);
    const onSnapshot = vi.fn();
    const onConnection = vi.fn();
    cleanup = subscribeLiveState(onSnapshot, onConnection);
    await vi.advanceTimersByTimeAsync(0);
    expect(onSnapshot).toHaveBeenCalledWith(snapshot);
    expect(onConnection).toHaveBeenCalledWith('offline');
  });

  it('retries a poll that times out', async () => {
    vi.mocked(fetchState).mockImplementation((signal) => new Promise((_, reject) => {
      signal!.addEventListener('abort', () => reject(new Error('aborted')));
    }));
    cleanup = subscribeLiveState(vi.fn(), vi.fn());
    Stream.instances[0]!.onerror!();
    await vi.advanceTimersByTimeAsync(12_000);
    expect(fetchState).toHaveBeenCalledTimes(2);
  });
});
