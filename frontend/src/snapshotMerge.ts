import type { ItemView, JobView, Snapshot } from './types';

/**
 * Applying a frame to what the browser already holds.
 *
 * Almost nothing in a queue changes from one second to the next: a thousand
 * finished files say exactly what they said before. So a frame carries every
 * job whole — its counts, its size, its rate, all of which are small — and
 * inside it only the rows that actually moved, marked with `patch`. This is
 * where the rest is filled back in, so nothing downstream has to know that
 * the wire is thrifty: the search, the progress panel and the cards all see
 * complete lists.
 *
 * A job whose rows have come or gone arrives whole instead, because a merge
 * cannot express a removal. So can the first frame of a connection, and any
 * frame after one was dropped on the way here. That is what keeps this
 * self-healing: a client can always be handed the truth outright, and is
 * whenever a patch would not reach it.
 */
export function mergeSnapshot(previous: Snapshot | null, incoming: Snapshot): Snapshot {
  if (!incoming.jobs.some((job) => job.patch)) return incoming;

  const held = new Map(previous?.jobs.map((job) => [job.id, job.items]) ?? []);
  return {
    ...incoming,
    jobs: incoming.jobs.map((job) => {
      if (!job.patch) return job;
      const { patch: _patch, ...rest } = job;
      return { ...rest, items: mergeItems(held.get(job.id) ?? [], job.items) };
    }),
  };
}

/**
 * Folds changed rows into the ones already held, in place.
 *
 * Order is the job's own and a patch does not restate it, so a row that is
 * already known is replaced where it stands rather than moved to the end. A
 * row that is not known is new and goes after the rest — which only happens
 * where the whole list would otherwise have been sent, so it is a belt to
 * the braces rather than the usual path.
 */
function mergeItems(held: readonly ItemView[], changed: readonly ItemView[]): ItemView[] {
  if (changed.length === 0) return held as ItemView[];

  const at = new Map(held.map((item, index) => [item.id, index]));
  const merged = [...held];
  for (const item of changed) {
    const index = at.get(item.id);
    if (index === undefined) {
      at.set(item.id, merged.length);
      merged.push(item);
    } else {
      merged[index] = item;
    }
  }
  return merged;
}

/** Re-exported for the type's sake, so callers need not reach for JobView. */
export type { JobView };
