import type { ItemView, JobView, Snapshot } from './types';

/**
 * A light progression layer over the download stats: enough to make a long
 * queue feel like it is going somewhere, without getting in the way of the
 * tool. Everything here is derived from bytes actually written to disk.
 */

/** Cumulative session counters, persisted across reloads. */
export interface Progress {
  filesCompleted: number;
  bytesDownloaded: number;
  peakSpeed: number;
  maxParallel: number;
  hostsUsed: string[];
  unlocked: string[];
  /** Ids of items already counted, so a re-render never double-counts. */
  countedItems: string[];
}

export const EMPTY_PROGRESS: Progress = {
  filesCompleted: 0,
  bytesDownloaded: 0,
  peakSpeed: 0,
  maxParallel: 0,
  hostsUsed: [],
  unlocked: [],
  countedItems: [],
};

/** A rank, reached at a cumulative byte total. */
export interface Level {
  readonly index: number;
  readonly name: string;
  readonly at: number;
}

const GB = 1_000_000_000;

export const LEVELS: readonly Level[] = [
  { index: 1, name: 'Novice', at: 0 },
  { index: 2, name: 'Fetcher', at: 0.25 * GB },
  { index: 3, name: 'Collector', at: 1 * GB },
  { index: 4, name: 'Hoarder', at: 5 * GB },
  { index: 5, name: 'Archivist', at: 20 * GB },
  { index: 6, name: 'Curator', at: 75 * GB },
  { index: 7, name: 'Librarian', at: 250 * GB },
  { index: 8, name: 'Datavore', at: 1000 * GB },
  { index: 9, name: 'Warehouse', at: 5000 * GB },
  { index: 10, name: 'Singularity', at: 20000 * GB },
];

/** Where the session sits between two ranks. */
export interface Standing {
  readonly level: Level;
  readonly next: Level | null;
  /** 0-1 through the current rank; 1 at the final rank. */
  readonly fraction: number;
  readonly toNext: number;
}

export function standing(bytes: number): Standing {
  let level = LEVELS[0]!;
  for (const candidate of LEVELS) {
    if (bytes >= candidate.at) level = candidate;
  }
  const next = LEVELS[level.index] ?? null;
  if (!next) return { level, next: null, fraction: 1, toNext: 0 };

  const span = next.at - level.at;
  const gained = bytes - level.at;
  return {
    level,
    next,
    fraction: span > 0 ? Math.min(1, Math.max(0, gained / span)) : 0,
    toNext: Math.max(0, next.at - bytes),
  };
}

/** An unlockable badge. */
export interface Achievement {
  readonly id: string;
  readonly icon: string;
  readonly title: string;
  readonly detail: string;
  readonly earned: (p: Progress, hostCount: number) => boolean;
}

export const ACHIEVEMENTS: readonly Achievement[] = [
  {
    id: 'first-blood',
    icon: '🎯',
    title: 'First Contact',
    detail: 'Finish your first download',
    earned: (p) => p.filesCompleted >= 1,
  },
  {
    id: 'ten-files',
    icon: '📦',
    title: 'Getting Warmed Up',
    detail: 'Finish 10 files',
    earned: (p) => p.filesCompleted >= 10,
  },
  {
    id: 'hundred-files',
    icon: '🏗️',
    title: 'Bulk Operator',
    detail: 'Finish 100 files',
    earned: (p) => p.filesCompleted >= 100,
  },
  {
    id: 'one-gig',
    icon: '💾',
    title: 'Gigabyte Club',
    detail: 'Download 1 GB in total',
    earned: (p) => p.bytesDownloaded >= GB,
  },
  {
    id: 'ten-gigs',
    icon: '🗄️',
    title: 'Serious Storage',
    detail: 'Download 10 GB in total',
    earned: (p) => p.bytesDownloaded >= 10 * GB,
  },
  {
    id: 'fast-50',
    icon: '⚡',
    title: 'Quick Draw',
    detail: 'Hit 50 MB/s',
    earned: (p) => p.peakSpeed >= 50_000_000,
  },
  {
    id: 'fast-200',
    icon: '🚀',
    title: 'Ludicrous Speed',
    detail: 'Hit 200 MB/s',
    earned: (p) => p.peakSpeed >= 200_000_000,
  },
  {
    id: 'parallel-8',
    icon: '🐙',
    title: 'Many Hands',
    detail: 'Run 8 transfers at once',
    earned: (p) => p.maxParallel >= 8,
  },
  {
    id: 'globetrotter',
    icon: '🌍',
    title: 'Globetrotter',
    detail: 'Download from every supported host',
    earned: (p, hostCount) => hostCount > 0 && p.hostsUsed.length >= hostCount,
  },
];

/**
 * Folds a snapshot into the running totals. Completed items are counted once,
 * keyed by id, so repeated snapshots of the same state are idempotent.
 */
export function accumulate(previous: Progress, snapshot: Snapshot): Progress {
  const counted = new Set(previous.countedItems);
  let files = previous.filesCompleted;
  let bytes = previous.bytesDownloaded;
  const hosts = new Set(previous.hostsUsed);

  const liveIds = new Set<string>();
  for (const job of snapshot.jobs) {
    for (const item of job.items) {
      liveIds.add(item.id);
      if (item.status !== 'done' || counted.has(item.id)) continue;
      counted.add(item.id);
      files += 1;
      bytes += bytesOf(item);
      hosts.add(job.host);
    }
  }

  // Forget ids for jobs the user cleared, so the set cannot grow forever.
  // Re-adding a cleared job legitimately counts as a fresh download.
  const retained = previous.countedItems.filter((id) => liveIds.has(id));
  for (const id of counted) {
    if (liveIds.has(id) && !retained.includes(id)) retained.push(id);
  }

  return {
    filesCompleted: files,
    bytesDownloaded: bytes,
    peakSpeed: Math.max(previous.peakSpeed, snapshot.speed),
    maxParallel: Math.max(previous.maxParallel, snapshot.active),
    hostsUsed: [...hosts],
    unlocked: previous.unlocked,
    countedItems: retained,
  };
}

function bytesOf(item: ItemView): number {
  return item.downloaded > 0 ? item.downloaded : Math.max(0, item.size);
}

/** Achievements newly satisfied but not yet recorded. */
export function newlyUnlocked(progress: Progress, hostCount: number): Achievement[] {
  const have = new Set(progress.unlocked);
  return ACHIEVEMENTS.filter((a) => !have.has(a.id) && a.earned(progress, hostCount));
}

/** Completed-vs-total across every job, for the session ring. */
export function jobTotals(jobs: JobView[]): { done: number; total: number } {
  return jobs.reduce(
    (acc, job) => ({ done: acc.done + job.done, total: acc.total + job.total }),
    { done: 0, total: 0 },
  );
}
