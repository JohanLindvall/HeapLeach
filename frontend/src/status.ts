// SPDX-License-Identifier: MIT
import type { ItemView, JobView, Status } from './types';

/**
 * The status vocabulary, in one place.
 *
 * The sidebar filters, card actions and header's "Clear finished" count
 * share these predicates so their definitions stay together. Clearing a
 * job requires every file to be complete, beyond merely being terminal.
 */

/** Still moving, or waiting to: a cancel makes sense here. */
export function isActive(status: Status): boolean {
  return status === 'running' || status === 'queued' || status === 'resolving';
}

/** Over, one way or another: nothing further will happen on its own. */
export function isTerminal(status: Status): boolean {
  return status === 'done' || status === 'failed' || status === 'canceled';
}

/**
 * Safe to clear: every file completed, with no source error and no listing
 * a failure cut short. Mirrors Job.complete.
 */
export function isComplete(job: JobView): boolean {
  return job.status === 'done' && !job.error && !job.incomplete && job.total > 0 && job.done === job.total;
}

/** Stopped short: a retry makes sense here. */
export function isRetryable(status: Status): boolean {
  return status === 'failed' || status === 'canceled';
}

/**
 * Whether a job's card offers a retry: it stopped short, or its listing was
 * cut short by a failure and nothing of it is still moving, since retrying
 * that reads the source again for what was left out.
 */
export function offersRetry(job: JobView): boolean {
  return isRetryable(job.status) || (job.incomplete === true && !isActive(job.status));
}

/** The status groups the sidebar filters by. */
export type Filter = 'all' | 'active' | 'done' | 'failed';

export const FILTERS: readonly Filter[] = ['all', 'active', 'done', 'failed'];

/** Which filter a job belongs to. */
export function matchesFilter(job: JobView, filter: Filter): boolean {
  switch (filter) {
    case 'active':
      return isActive(job.status);
    // A finished job whose listing a failure cut short sits with the failed
    // ones: like them, it is what a retry is for.
    case 'done':
      return job.status === 'done' && !offersRetry(job);
    case 'failed':
      return offersRetry(job);
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

/**
 * The phases a file can be in, which the sidebar's second section filters
 * by. They cut across jobs: "everything that failed" is one list however
 * many albums it came from.
 */
export type Phase = 'running' | 'queued' | 'failed' | 'done';

export const PHASES: readonly { key: Phase; label: string }[] = [
  { key: 'running', label: 'Downloading' },
  { key: 'queued', label: 'Queued' },
  { key: 'failed', label: 'Failed' },
  { key: 'done', label: 'Done' },
];

/**
 * Which phase a file is in. A cancelled file sits with the failed ones —
 * both stopped short and both are what a retry is for — and a file has no
 * phase while its job is still being read.
 */
export function phaseOf(status: Status): Phase | null {
  switch (status) {
    case 'running':
    case 'queued':
    case 'done':
      return status;
    case 'failed':
    case 'canceled':
      return 'failed';
    default:
      return null;
  }
}

/** A file, with the job it belongs to. */
export interface FileEntry {
  readonly item: ItemView;
  readonly job: JobView;
}

/**
 * How many files are in each phase, in one pass and without building a list:
 * the sidebar wants only the numbers, every second, and a queue can hold a
 * few hundred thousand files.
 */
export function countPhases(jobs: readonly JobView[]): Record<Phase, number> {
  const counts: Record<Phase, number> = { running: 0, queued: 0, failed: 0, done: 0 };
  for (const job of jobs) {
    for (const item of job.items) {
      const phase = phaseOf(item.status);
      if (phase) counts[phase] += 1;
    }
  }
  return counts;
}

/**
 * The files in one phase, in queue order: jobs as the snapshot lists them,
 * files as each job does. Built only for the phase on screen.
 */
export function filesInPhase(jobs: readonly JobView[], phase: Phase): FileEntry[] {
  const out: FileEntry[] = [];
  for (const job of jobs) {
    for (const item of job.items) {
      if (phaseOf(item.status) === phase) out.push({ item, job });
    }
  }
  return out;
}

/** Case-insensitive match against a file's name, folder and job. */
export function matchesFileQuery(entry: FileEntry, query: string): boolean {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return (
    entry.item.name.toLowerCase().includes(q) ||
    (entry.item.dir ?? '').toLowerCase().includes(q) ||
    entry.job.title.toLowerCase().includes(q)
  );
}
