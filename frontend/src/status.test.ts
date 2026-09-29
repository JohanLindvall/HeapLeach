import { describe, expect, it } from 'vitest';
import {
  countByFilter,
  countPhases,
  filesInPhase,
  isActive,
  isRetryable,
  isTerminal,
  matchesFilter,
  matchesFileQuery,
  matchesQuery,
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
    createdAt: '2026-01-01T00:00:00Z',
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

describe('matchesFilter', () => {
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
