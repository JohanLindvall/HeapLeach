import { useCallback, useEffect, useLayoutEffect, useRef, useState, type RefObject } from 'react';

/**
 * Below this many files a list is rendered whole. Windowing costs something
 * of its own — find-in-page only sees what is mounted — and an ordinary
 * album is nowhere near large enough to be worth paying it.
 */
const VIRTUAL_FROM = 120;

/** How far past the viewport rows are kept, so a scroll has slack. */
const OVERSCAN_PX = 800;

/** Height taken for a row nothing has measured yet, corrected on sight. */
const ASSUMED_ROW_PX = 72;

/** Which slice of a long list is worth having in the document. */
export interface VirtualRows {
  /** First row to render. */
  readonly start: number;
  /** One past the last row to render. */
  readonly end: number;
  /** Height standing in for the rows before `start`. */
  readonly padTop: number;
  /** Height standing in for the rows after `end`. */
  readonly padBottom: number;
}

const NONE: VirtualRows = { start: 0, end: 0, padTop: 0, padBottom: 0 };

/**
 * Keeps only the rows around the viewport in the document.
 *
 * A single job can hold tens of thousands of files, and every row is a dozen
 * elements or so. Hand the lot to the browser and it spends its time on
 * style and layout for rows nobody is looking at: the tab goes unresponsive
 * long before the download finishes. What the page holds instead is the band
 * around the viewport, with everything else standing in as two spacers, so
 * the scrollbar still measures the whole list.
 *
 * Rows are not all the same height — an error or a wait note adds a line —
 * so heights are learned by measuring what is on screen and remembered. A
 * row not yet seen is taken to be the average of the ones that have been,
 * which is close enough that learning otherwise shifts what is below it by a
 * pixel or two rather than visibly.
 *
 * Returns null for a list short enough to render whole.
 */
export function useVirtualRows(
  ref: RefObject<HTMLElement | null>,
  count: number,
): VirtualRows | null {
  const virtual = count >= VIRTUAL_FROM;

  // Measured heights, sparse: a hole is a row that has never been rendered.
  const heights = useRef<number[]>([]);
  // Running total and tally of the measured ones, for the average that
  // stands in for the rest.
  const measured = useRef({ total: 0, rows: 0 });
  // offsets[i] is where row i starts; offsets[count] is the whole height.
  const offsets = useRef<number[]>([0]);
  // The width every remembered height was measured at.
  const width = useRef(0);

  const [rows, setRows] = useState<VirtualRows>(NONE);
  // What the document is actually showing. Held in a ref so measuring does
  // not have to rebuild its callback every time the slice moves.
  const showing = useRef(rows);
  showing.current = virtual ? rows : NONE;

  const settle = useCallback((): void => {
    const el = ref.current;
    if (!el) return;

    // A row's height is a fact about a width: a narrower window wraps the
    // name and the meta line, and every remembered height is then wrong.
    // Forgetting them costs a re-measure on the way past, which is what
    // would have happened anyway had the list been opened at this size.
    if (el.clientWidth !== width.current) {
      width.current = el.clientWidth;
      heights.current.length = 0;
      measured.current = { total: 0, rows: 0 };
    }

    const h = heights.current;
    let learned = h.length !== count;
    h.length = count;

    // Measure every row on screen. They sit between the two spacers, so
    // which row each element is needs no asking.
    const { start, end, padTop } = showing.current;
    const lead = padTop > 0 ? 1 : 0;
    for (let i = start; i < end; i++) {
      const row = el.children[lead + i - start] as HTMLElement | undefined;
      if (!row) break;
      const height = row.offsetHeight;
      if (height <= 0 || h[i] === height) continue;
      const before = h[i] ?? 0;
      measured.current.total += height - before;
      if (before === 0) measured.current.rows += 1;
      h[i] = height;
      learned = true;
    }

    if (learned || offsets.current.length !== count + 1) {
      const { total, rows: seen } = measured.current;
      const assumed = seen > 0 ? total / seen : ASSUMED_ROW_PX;
      const next = offsets.current;
      next.length = count + 1;
      next[0] = 0;
      for (let i = 0; i < count; i++) next[i + 1] = next[i]! + (h[i] ?? assumed);
    }

    // The document is what scrolls, so the band worth keeping is the
    // viewport and its overscan, read in the list's own coordinates.
    const top = el.getBoundingClientRect().top;
    const view = window.innerHeight || document.documentElement.clientHeight;
    const first = rowAt(offsets.current, -top - OVERSCAN_PX, count);
    const last = Math.min(count, rowAt(offsets.current, -top + view + OVERSCAN_PX, count) + 1);

    const slice: VirtualRows = {
      start: first,
      end: last,
      padTop: offsets.current[first]!,
      padBottom: offsets.current[count]! - offsets.current[last]!,
    };
    const shown = showing.current;
    if (
      slice.start !== shown.start ||
      slice.end !== shown.end ||
      slice.padTop !== shown.padTop ||
      slice.padBottom !== shown.padBottom
    ) {
      setRows(slice);
    }
  }, [ref, count]);

  // After every render, so a row that just grew — an error arriving, say —
  // is accounted for before the frame is painted rather than after it.
  useLayoutEffect(() => {
    if (virtual) settle();
  });

  useEffect(() => {
    if (!virtual) return;
    let frame = 0;
    const follow = (): void => {
      if (frame !== 0) return;
      frame = window.requestAnimationFrame(() => {
        frame = 0;
        settle();
      });
    };
    window.addEventListener('scroll', follow, { passive: true });
    window.addEventListener('resize', follow);
    return () => {
      window.cancelAnimationFrame(frame);
      window.removeEventListener('scroll', follow);
      window.removeEventListener('resize', follow);
    };
  }, [virtual, settle]);

  return virtual ? rows : null;
}

/** The last row starting at or before `y`. */
function rowAt(offsets: number[], y: number, count: number): number {
  if (y <= 0 || count === 0) return 0;
  let low = 0;
  let high = count - 1;
  while (low < high) {
    const mid = (low + high + 1) >> 1;
    if (offsets[mid]! <= y) low = mid;
    else high = mid - 1;
  }
  return low;
}
