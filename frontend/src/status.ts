import type { JobView, Status } from './types';

/**
 * The status vocabulary, in one place.
 *
 * Every view asks the same three questions of a status — is it still going
 * to change, can it be tried again, is it over — and each used to answer
 * with its own list of literals. The sidebar's "Active" group, the card's
 * cancel button and the header's "Clear finished" count were three copies of
 * one idea, which is how they come to disagree.
 */

/** Still moving, or waiting to: a cancel makes sense here. */
export function isActive(status: Status): boolean {
  return status === 'running' || status === 'queued' || status === 'resolving';
}

/** Over, one way or another: nothing further will happen on its own. */
export function isTerminal(status: Status): boolean {
  return status === 'done' || status === 'failed' || status === 'canceled';
}

/** Stopped short: a retry makes sense here. */
export function isRetryable(status: Status): boolean {
  return status === 'failed' || status === 'canceled';
}

/** The status groups the sidebar filters by. */
export type Filter = 'all' | 'active' | 'done' | 'failed';

export const FILTERS: readonly Filter[] = ['all', 'active', 'done', 'failed'];

/** Which filter a job belongs to. */
export function matchesFilter(job: JobView, filter: Filter): boolean {
  switch (filter) {
    case 'active':
      return isActive(job.status);
    case 'done':
      return job.status === 'done';
    case 'failed':
      return isRetryable(job.status);
    default:
      return true;
  }
}

/** How many jobs each filter would show, counted in one pass. */
export function countByFilter(jobs: readonly JobView[]): Record<Filter, number> {
  const counts: Record<Filter, number> = { all: 0, active: 0, done: 0, failed: 0 };
  for (const job of jobs) {
    for (const filter of FILTERS) {
      if (matchesFilter(job, filter)) counts[filter] += 1;
    }
  }
  return counts;
}

/** Case-insensitive match against everything a user knows a job by. */
export function matchesQuery(job: JobView, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return (
    job.title.toLowerCase().includes(q) ||
    job.source.toLowerCase().includes(q) ||
    job.host.toLowerCase().includes(q) ||
    job.items.some((item) => item.name.toLowerCase().includes(q))
  );
}
