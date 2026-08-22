import { useEffect, useRef, useState } from 'react';

/** How many samples the graph keeps; at ~400 ms a tick this is ~40 seconds. */
const HISTORY = 100;

/**
 * Keeps a rolling window of throughput samples for the header graph.
 *
 * The snapshot only ever carries the current rate, so the history has to be
 * accumulated on this side. A ref holds the samples and state is replaced
 * once per tick, which keeps the graph animating without re-rendering the
 * whole tree on every sample.
 */
export function useSpeedHistory(speed: number): number[] {
  const samples = useRef<number[]>([]);
  const [series, setSeries] = useState<number[]>([]);

  useEffect(() => {
    const next = [...samples.current, speed];
    samples.current = next.length > HISTORY ? next.slice(-HISTORY) : next;
    setSeries(samples.current);
  }, [speed]);

  return series;
}
