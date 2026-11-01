// SPDX-License-Identifier: MIT
import { describe, expect, it } from 'vitest';
import {
  countByFilter,
  countPhases,
  filesInPhase,
  isActive,
  isComplete,
  isRetryable,
  isTerminal,
  matchesFilter,
  matchesFileQuery,
  matchesQuery,
  offersRetry,
  phaseOf,
} from './status';
import type { JobView, Status } from './types';

const ALL: Status[] = ['resolving', 'queued', 'running', 'done', 'failed', 'canceled'];

function job(status: Status, extra: Partial<JobView> = {}): JobView {
  return {
    id: status,
    source: 'https://example.test/' + status,
    title: 'Album ' + status,
    host: 'example',
    status,
    items: [],
    total: 0,
    done: 0,
    failed: 0,
    canceled: 0,
    active: 0,
    size: 0,
    downloaded: 0,
    speed: 0,
    sizeKnown: true,
    ...extra,
  };
}

describe('status predicates', () => {
  it('partition every status into active or terminal, never both', () => {
    for (const status of ALL) {
      expect(isActive(status) !== isTerminal(status), status).toBe(true);
    }
  });

  it('only allow a retry of something that stopped short', () => {
    expect(ALL.filter(isRetryable)).toEqual(['failed', 'canceled']);
  });
});

describe('isComplete', () => {
  it('only clears completed jobs, keeping failed, canceled and active jobs', () => {
    const jobs = ALL.map((status) => job(status, { total: 2, done: 2 }));
    expect(jobs.filter(isComplete).map((j) => j.status)).toEqual(['done']);
  });

  it('keeps done jobs with canceled files or source errors', () => {
    expect(isComplete(job('done', { total: 2, done: 1, canceled: 1 }))).toBe(false);
    expect(isComplete(job('done', { total: 1, done: 1, error: 'Listing incomplete' }))).toBe(false);
  });

  it('keeps a done job whose listing a failure cut short', () => {
    expect(isComplete(job('done', { total: 1, done: 1, incomplete: true }))).toBe(false);
  });

  it('keeps empty jobs', () => {
    expect(isComplete(job('queued'))).toBe(false);
    expect(isComplete(job('done'))).toBe(false);
  });
});

describe('offersRetry', () => {
  it('offers a retry of a job cut short once nothing of it is moving', () => {
    expect(ALL.filter((s) => offersRetry(job(s, { incomplete: true })))).toEqual(['done', 'failed', 'canceled']);
    expect(ALL.filter((s) => offersRetry(job(s)))).toEqual(['failed', 'canceled']);
  });
});

describe('matchesFilter', () => {
  it('files a done job whose listing was cut short under "failed", where its retry is', () => {
    const cut = job('done', { total: 1, done: 1, incomplete: true });
    expect(matchesFilter(cut, 'failed')).toBe(true);
    expect(matchesFilter(cut, 'done')).toBe(false);
    expect(countByFilter([cut])).toEqual({ all: 1, active: 0, done: 0, failed: 1 });
  });

  it('shows everything under "all"', () => {
    for (const status of ALL) expect(matchesFilter(job(status), 'all')).toBe(true);
  });

  it('files each job under exactly one of the other groups', () => {
    for (const status of ALL) {
      const groups = (['active', 'done', 'failed'] as const).filter((f) =>
        matchesFilter(job(status), f),
      );
      expect(groups, status).toHaveLength(1);
    }
  });
});

describe('countByFilter', () => {
  it('counts in one pass what the filters would show', () => {
    const jobs = ALL.map((status) => job(status));
    expect(countByFilter(jobs)).toEqual({ all: 6, active: 3, done: 1, failed: 2 });
  });

  it('is all zeroes for no jobs', () => {
    expect(countByFilter([])).toEqual({ all: 0, active: 0, done: 0, failed: 0 });
  });
});

describe('matchesQuery', () => {
  const j = job('done', {
    title: 'Summer Mixtape',
    source: 'https://files.example.test/d/AbCd',
    host: 'example',
    items: [
      {
        id: 'i1',
        name: 'First Song.mp3',
        status: 'done',
        size: 1,
        downloaded: 1,
        speed: 0,
      },
    ],
  });

  it('matches everything with an empty or blank query', () => {
    expect(matchesQuery(j, '')).toBe(true);
    expect(matchesQuery(j, '   ')).toBe(true);
  });

  it('matches the title, source, host and file names, ignoring case', () => {
    expect(matchesQuery(j, 'MIXTAPE')).toBe(true);
    expect(matchesQuery(j, 'files.example.test')).toBe(true);
    expect(matchesQuery(j, 'Example')).toBe(true);
    expect(matchesQuery(j, 'first song')).toBe(true);
  });

  it('rejects what appears nowhere', () => {
    expect(matchesQuery(j, 'winter')).toBe(false);
  });
});

describe('file phases', () => {
  it('puts every status but resolving in exactly one phase', () => {
    expect(ALL.map(phaseOf)).toEqual([null, 'queued', 'running', 'done', 'failed', 'failed']);
  });

  it('sorts files across jobs, keeping queue order', () => {
    const item = (id: string, status: Status) => ({ id, name: id + '.bin', status, size: 1, downloaded: 0, speed: 0 });
    const a = job('running', { id: 'a', items: [item('a1', 'done'), item('a2', 'running'), item('a3', 'canceled')] });
    const b = job('failed', { id: 'b', items: [item('b1', 'failed'), item('b2', 'done')] });
    expect(filesInPhase([a, b], 'done').map((e) => e.item.id)).toEqual(['a1', 'b2']);
    expect(filesInPhase([a, b], 'running').map((e) => e.item.id)).toEqual(['a2']);
    const failed = filesInPhase([a, b], 'failed');
    expect(failed.map((e) => e.item.id)).toEqual(['a3', 'b1']);
    expect(failed[1]!.job.id).toBe('b');
    expect(filesInPhase([a, b], 'queued')).toEqual([]);
    expect(countPhases([a, b])).toEqual({ running: 1, queued: 0, failed: 2, done: 2 });
  });

  it('matches a file by its name, its folder or its job', () => {
    const entry = {
      item: { id: 'x', name: 'First Song.mp3', dir: 'A Band/First Album', status: 'done' as Status, size: 1, downloaded: 1, speed: 0 },
      job: job('done', { title: 'A Band - First Album' }),
    };
    expect(matchesFileQuery(entry, 'song')).toBe(true);
    expect(matchesFileQuery(entry, 'first album')).toBe(true);
    expect(matchesFileQuery(entry, 'a band -')).toBe(true);
    expect(matchesFileQuery(entry, 'nothing')).toBe(false);
    expect(matchesFileQuery(entry, '  ')).toBe(true);
  });
});
