import { useEffect, useState } from 'react';

/** How many samples the graph keeps; at ~400 ms a tick this is ~40 seconds. */
const HISTORY = 100;

/**
 * Keeps a rolling window of throughput samples for the header graph.
 *
 * The snapshot only ever carries the current rate, so the history has to be
 * accumulated on this side: one sample appended per change, the oldest
 * dropped past the window.
 */
export function useSpeedHistory(speed: number): number[] {
  const [series, setSeries] = useState<number[]>([]);

  useEffect(() => {
    setSeries((previous) => {
      const next = [...previous, speed];
      return next.length > HISTORY ? next.slice(-HISTORY) : next;
    });
  }, [speed]);

  return series;
}
